package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExecuteSearchKnowledge_LocalDispatchSubstantive proves the P3 hinge: the
// SearchKnowledge ReAct tool dispatches LOCALLY on the engine retriever (never
// through the external/safe executor — its Route is knowledge, not external_api),
// returns a SUBSTANTIVE EvidenceLedger (a chunk-content snippet the agent can
// ground an actionable answer on, not the content-free diagnosis ledger), and
// records the hits so the final-answer no-raw-leak guard can validate the
// synthesis turn.
func TestExecuteSearchKnowledge_LocalDispatchSubstantive(t *testing.T) {
	content := "降低 vLLM 显存占用：缩短上下文 --max-model-len；降低并发 --max-num-seqs；" +
		"多卡张量并行 --tensor-parallel-size；使用量化 quantization。"
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled: true,
		HitItems: []knowledge.RetrievalHit{{
			Kept:  true,
			Score: 90,
			Chunk: knowledge.KBChunk{
				ChunkID:    "ext-gpu-oom-vllm-001",
				Title:      "vLLM 显存不足 (OOM) 排查",
				SourceType: "external",
				Content:    content,
			},
		}},
	}}}
	exec := &mockExecutor{}
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}, exec, nil)
	eng.SetKnowledgeRetriever(retriever)
	eng.knowledgeQAAgentLoopThisTurn = true

	tc := openai.ToolCall{
		ID:   "call-sk",
		Type: openai.ToolTypeFunction,
		Function: openai.FunctionCall{
			Name:      "SearchKnowledge",
			Arguments: `{"query":"vllm 显存不足"}`,
		},
	}
	out := eng.executeTool(context.Background(), tc, noopStep)

	// Substantive evidence: chunk id + a real actionable token in the snippet.
	assert.Contains(t, out, "EvidenceLedger")
	assert.Contains(t, out, "ext-gpu-oom-vllm-001")
	assert.Contains(t, out, "--max-model-len", "result must carry actionable content for the agent to ground on")

	// Local dispatch: the external/safe tool executor was NEVER called.
	assert.Empty(t, exec.calls, "SearchKnowledge must dispatch locally, never via the API/safe executor")

	// Retriever ran with the query; hits recorded for the synthesis guard.
	require.Len(t, retriever.calls, 1)
	assert.Equal(t, "vllm 显存不足", retriever.calls[0].question)
	assert.True(t, eng.searchKnowledgeRanThisTurn)
	assert.Len(t, eng.searchKnowledgeHitsThisTurn, 1)
}

// A remote MCP outage is an operational result, not a corpus-gap result. The
// model receives a safe retry signal and the trace must not label it
// no_evidence, which would send operators to edit KB content for a network or
// readiness problem.
func TestExecuteSearchKnowledge_RemoteUnavailableIsDistinctFromEmpty(t *testing.T) {
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:       true,
		Empty:         true,
		Unavailable:   true,
		FailureReason: "mcp_unavailable",
	}}}
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)
	var traces []observability.RetrievalTrace
	eng.SetRetrievalTraceObserver(func(trace observability.RetrievalTrace) { traces = append(traces, trace) })

	out := eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "查询知识"}, noopStep)

	assert.Contains(t, out, `"knowledge_unavailable":true`)
	assert.Contains(t, out, "知识库服务暂时不可用")
	assert.True(t, eng.searchKnowledgeRanThisTurn)
	require.Len(t, traces, 1)
	assert.True(t, traces[0].Unavailable)
	assert.Equal(t, "mcp_unavailable", traces[0].FailureReason)
	assert.Empty(t, traces[0].RefusedReason)
}

func TestExecuteSearchKnowledge_PartialRemoteUnavailableIsVisible(t *testing.T) {
	eng, retriever := planningEngineWithConversation(t,
		`{"answer_question":"实例关机后还会产生哪些费用","search_queries":["关机后计费规则","数据盘关机是否计费"]}`,
		[]knowledge.RetrievalResult{
			{Enabled: true, Empty: true, Unavailable: true, FailureReason: "mcp_timeout"},
			{Enabled: true, Empty: true},
			// Third result is for the anchored Agent query. It stays available so
			// the partial-outage shape under test is still one unavailable query
			// out of the fan-out, not two.
			{Enabled: true, Empty: true},
		},
	)

	out := eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "关机后还收什么"}, noopStep)

	require.Len(t, retriever.calls, 3)
	assert.Contains(t, out, `"knowledge_unavailable":true`)
	assert.Contains(t, out, `"partial":true`)
	assert.Contains(t, out, `"unavailable_queries":1`)
}

func TestExecuteSearchKnowledge_MultipleCallsPreserveActivityIDsInCitationTrace(t *testing.T) {
	chunkA := knowledge.KBChunk{
		ChunkID:   "chunk-a",
		KBVersion: "kb.v1",
		Title:     "GPU resize",
		Content:   "调整 GPU 数量需要先关机。",
	}
	chunkB := knowledge.KBChunk{
		ChunkID:   "chunk-b",
		KBVersion: "kb.v1",
		Title:     "Disk persistence",
		Content:   "重装会清除系统盘，数据盘不受影响。",
	}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{
		{Enabled: true, KBVersion: "kb.v1", HitItems: []knowledge.RetrievalHit{{Kept: true, Score: 90, Chunk: chunkA}}},
		{Enabled: true, KBVersion: "kb.v1", HitItems: []knowledge.RetrievalHit{{Kept: true, Score: 91, Chunk: chunkB}}},
	}}
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)
	eng.knowledgeQAAgentLoopThisTurn = true
	var traces []observability.RetrievalTrace
	eng.SetRetrievalTraceObserver(func(trace observability.RetrievalTrace) {
		traces = append(traces, trace)
	})

	_ = eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "GPU 能否调整"}, noopStep)
	_ = eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "更换 GPU 数据会变吗"}, noopStep)
	assert.Equal(t, "GPU 能否调整", eng.resolvedKnowledgeQuestionThisTurn,
		"the first history-aware query is the stable question the answer must resolve")
	assert.Equal(t, "GPU 能否调整", eng.searchKnowledgeLedgerThisTurn.Query,
		"later subqueries must not turn the verifier input into a synthetic q1 | q2 question")
	assert.NotContains(t, eng.searchKnowledgeLedgerThisTurn.Query, " | ")
	report := knowledge.ValidateGroundedCitations("GPU 调整和数据保留分别见资料 [[chunk-a]] [[chunk-b]]。", eng.searchKnowledgeLedgerThisTurn)
	require.True(t, report.Grounded())
	eng.emitSearchKnowledgeCitationTrace(report)

	require.Len(t, retriever.calls, 2)
	require.NotEmpty(t, traces)
	final := traces[len(traces)-1]
	require.Len(t, final.Activities, 2)
	assert.Equal(t, "search_1", final.Activities[0].ID)
	assert.Equal(t, "search_2", final.Activities[1].ID)
	require.Len(t, final.References, 2)
	assert.Equal(t, []string{"search_1"}, final.References[0].ActivityIDs)
	assert.Equal(t, []string{"search_2"}, final.References[1].ActivityIDs)
	assert.Equal(t, []observability.RetrievalCitedRef{
		{RefID: "1", ChunkID: "chunk-a"},
		{RefID: "2", ChunkID: "chunk-b"},
	}, final.CitedRefs)
	assert.Equal(t, []string{"chunk-a", "chunk-b"}, final.CitedChunkIDs)
}

// TestExecuteSearchKnowledge_CallPastTheBudgetIsRejectedWithoutRetrieval keeps
// the original contract — past the per-turn call budget SearchKnowledge stops
// retrieving and says so — while no longer hard-coding the budget's value. The
// count moved (2 -> maxSearchKnowledgeCallsPerTurn) because the constant was
// deliberately raised; the assertion shape is unchanged, and binding the loop to
// the constant means removing the enforcement still fails this test.
func TestExecuteSearchKnowledge_CallPastTheBudgetIsRejectedWithoutRetrieval(t *testing.T) {
	results := make([]knowledge.RetrievalResult, 0, maxSearchKnowledgeCallsPerTurn)
	for i := 0; i < maxSearchKnowledgeCallsPerTurn; i++ {
		results = append(results, knowledge.RetrievalResult{Enabled: true})
	}
	retriever := &scriptedKnowledgeRetriever{results: results}
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)

	for i := 0; i < maxSearchKnowledgeCallsPerTurn; i++ {
		_ = eng.executeSearchKnowledge(context.Background(), map[string]any{"query": fmt.Sprintf("q%d", i)}, noopStep)
	}
	past := eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "past-budget"}, noopStep)

	require.Len(t, retriever.calls, maxSearchKnowledgeCallsPerTurn)
	assert.Contains(t, past, `"search_limit_reached":true`)
}

func TestSearchKnowledgeTurnTraceEmitsFullTurnEvidenceWithoutCitations(t *testing.T) {
	chunkA := knowledge.KBChunk{ChunkID: "chunk-a", KBVersion: "kb.v1", Title: "A", Content: "evidence a"}
	chunkB := knowledge.KBChunk{ChunkID: "chunk-b", KBVersion: "kb.v1", Title: "B", Content: "evidence b"}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{
		{Enabled: true, KBVersion: "kb.v1", HitItems: []knowledge.RetrievalHit{{Kept: true, Score: 90, Chunk: chunkA}}},
		{Enabled: true, KBVersion: "kb.v1", HitItems: []knowledge.RetrievalHit{{Kept: true, Score: 91, Chunk: chunkB}}},
	}}
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)
	eng.knowledgeQAAgentLoopThisTurn = true
	var traces []observability.RetrievalTrace
	eng.SetRetrievalTraceObserver(func(trace observability.RetrievalTrace) { traces = append(traces, trace) })

	_ = eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "q1"}, noopStep)
	_ = eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "q2"}, noopStep)
	eng.emitSearchKnowledgeTurnTrace(nil)

	require.NotEmpty(t, traces)
	final := traces[len(traces)-1]
	assert.True(t, final.TurnAggregate)
	assert.Equal(t, 2, final.Hits)
	require.Len(t, final.Activities, 2)
	require.Len(t, final.References, 2)
	require.Len(t, final.HitItems, 2)
	assert.Empty(t, final.CitedChunkIDs)
}

// TestExecuteSearchKnowledge_RelevanceFloorDropsWeakHits proves the relevance floor:
// when the retriever returns only topically-IRRELEVANT top-K (qwen3-reranker score
// below weakEvidenceSemanticThreshold=0.5 — what a tool-ops symptom retrieves when
// the external KB is off and only platform docs are in the index), SearchKnowledge
// drops them to an EMPTY ledger so the agent gives honest general guidance instead of
// false-grounding on irrelevant chunks. Verified live: relevant ext-* hits score
// 0.60-0.99 (kept); irrelevant platform hits at external-off score 0.01-0.07 (dropped).
// RerankerMode is set: the 0.07 is a genuine low reranker relevance score, so the
// floor legitimately applies (the reranker actually scored these hits — distinct
// from a reranker fallback, where a ~0.03 RRF-fusion score must NOT be floored).
func TestExecuteSearchKnowledge_RelevanceFloorDropsWeakHits(t *testing.T) {
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:      true,
		HybridMode:   "qwen3_rrf",
		RerankerMode: "qwen3-reranker-8b",
		HitItems: []knowledge.RetrievalHit{{
			Kept:  true,
			Score: 0.07, // below the 0.5 semantic floor — topically irrelevant top-K
			Chunk: knowledge.KBChunk{
				ChunkID:    "w0-resource_purchase-irrelevant",
				Title:      "购买资源",
				SourceType: "platform",
				Content:    "无关平台文档：如何购买资源与计费说明，与 vllm 进程被 kill 无关。",
			},
		}},
	}}}
	exec := &mockExecutor{}
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}, exec, nil)
	eng.SetKnowledgeRetriever(retriever)
	eng.knowledgeQAAgentLoopThisTurn = true

	tc := openai.ToolCall{
		ID:       "call-sk",
		Type:     openai.ToolTypeFunction,
		Function: openai.FunctionCall{Name: "SearchKnowledge", Arguments: `{"query":"vllm 进程被 kill"}`},
	}
	out := eng.executeTool(context.Background(), tc, noopStep)

	// The weak (irrelevant) hit is NOT in the ledger the agent sees — no false-grounding.
	assert.NotContains(t, out, "w0-resource_purchase-irrelevant", "weak hit must be dropped from the agent's ledger")
	// And it is NOT recorded as evidence the agent grounded on (the echo telemetry
	// would otherwise be judged against content the agent never received).
	assert.Empty(t, eng.searchKnowledgeHitsThisTurn, "weak hits must not be recorded as grounding evidence")
	// SearchKnowledge still ran (the raw retrieval is traced as weak for observability).
	assert.True(t, eng.searchKnowledgeRanThisTurn)
}

// TestExecuteSearchKnowledge_RerankerFallbackKeepsHits is the counterpart: when
// qwen3_rrf's reranker falls back (RerankerMode empty, RerankerFallbackReason
// set), the label stays qwen3_rrf but Score is the RRF-fusion value (~0.03). The
// 0.5 reranker floor must NOT fire — otherwise every fallback query empties the
// ledger and the agent fabricates from prior (the floor_reranker probe's failure).
// The fused top-k is kept so the agent grounds on a degraded-but-real ledger.
func TestExecuteSearchKnowledge_RerankerFallbackKeepsHits(t *testing.T) {
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:                true,
		HybridMode:             "qwen3_rrf",
		RerankerFallbackReason: "reranker_timeout", // reranker did NOT score; scores are RRF-fusion
		HitItems: []knowledge.RetrievalHit{{
			Kept:  true,
			Score: 0.031, // RRF-fusion scale, far below 0.5 — but not a relevance signal
			Chunk: knowledge.KBChunk{
				ChunkID:    "v2-resource_purchase-ac94d9679403ee37",
				Title:      "套餐包规格及扣除模式",
				SourceType: "platform",
				Content:    "Coding Plan 档位：Mini/Lite/Basic/Pro/Max/Ultra。",
			},
		}},
	}}}
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)
	eng.knowledgeQAAgentLoopThisTurn = true

	tc := openai.ToolCall{
		ID:       "call-sk",
		Type:     openai.ToolTypeFunction,
		Function: openai.FunctionCall{Name: "SearchKnowledge", Arguments: `{"query":"coding plan 套餐 档位 区别"}`},
	}
	out := eng.executeTool(context.Background(), tc, noopStep)

	// The chunk survives to the agent's ledger — no reranker-fallback blackout.
	assert.Contains(t, out, "v2-resource_purchase-ac94d9679403ee37", "reranker fallback must not empty the ledger")
	assert.Len(t, eng.searchKnowledgeHitsThisTurn, 1, "the fused hit is kept as grounding evidence on fallback")
}
