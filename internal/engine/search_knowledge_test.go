package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/tools"
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

// TestExecuteSearchKnowledge_RelevanceFloorDropsWeakHits proves the relevance floor:
// when the retriever returns only topically-IRRELEVANT top-K (qwen3-reranker score
// below weakEvidenceSemanticThreshold=0.5 — what a tool-ops symptom retrieves when
// the external KB is off and only platform docs are in the index), SearchKnowledge
// drops them to an EMPTY ledger so the agent gives honest general guidance instead of
// false-grounding on irrelevant chunks. Verified live: relevant ext-* hits score
// 0.60-0.99 (kept); irrelevant platform hits at external-off score 0.01-0.07 (dropped).
func TestExecuteSearchKnowledge_RelevanceFloorDropsWeakHits(t *testing.T) {
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:    true,
		HybridMode: "qwen3_rrf",
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

	tc := openai.ToolCall{
		ID:   "call-sk",
		Type: openai.ToolTypeFunction,
		Function: openai.FunctionCall{Name: "SearchKnowledge", Arguments: `{"query":"vllm 进程被 kill"}`},
	}
	out := eng.executeTool(context.Background(), tc, noopStep)

	// The weak (irrelevant) hit is NOT in the ledger the agent sees — no false-grounding.
	assert.NotContains(t, out, "w0-resource_purchase-irrelevant", "weak hit must be dropped from the agent's ledger")
	// And it is NOT recorded as evidence the agent grounded on (the no-raw-leak guard
	// would otherwise validate against content the agent never received).
	assert.Empty(t, eng.searchKnowledgeHitsThisTurn, "weak hits must not be recorded as grounding evidence")
	// SearchKnowledge still ran (the raw retrieval is traced as weak for observability).
	assert.True(t, eng.searchKnowledgeRanThisTurn)
}

// TestPlannerDiagnosis_DeadEndRelaxedWhenAgenticOn proves the P4a flag-gated
// relax: with COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE on, an empty-target diagnosis
// turn (>1 instance) NO LONGER short-circuits with the canned which-instance
// reply — it falls through to the agent lane (ReAct) so the loop can call
// SearchKnowledge first. Flag off is byte-identical (covered by
// TestPlannerDiagnosisClarificationDoesNotRequireEnabledIntent).
func TestPlannerDiagnosis_DeadEndRelaxedWhenAgenticOn(t *testing.T) {
	tools.SetAgenticSearchKnowledgeEnabled(true)
	defer tools.SetAgenticSearchKnowledgeEnabled(false)

	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{Plan: diagnosisPlanWithoutTarget()}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "react path"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-a", "Name": "train-a", "State": "Running"},
			map[string]any{"UHostId": "uhost-b", "Name": "train-b", "State": "Running"},
		},
	}, "test"))
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentResourceInfo},
		Model:          "deepseek-v4-flash",
	})

	// "我的机器 SSH 连不上了" = "my machine can't SSH"
	reply, err := eng.Chat(context.Background(), "我的机器 SSH 连不上了", noopStep)
	require.NoError(t, err)
	// "哪台实例" = "which instance" — the canned dead-end phrase.
	assert.NotContains(t, reply, "哪台实例", "flag on: must NOT fire the canned which-instance dead-end")
	assert.NotEmpty(t, mock.calls, "flag on: empty-target diagnosis falls through to the agent lane (ReAct)")
}

// TestGuardSearchKnowledgeSynthesis closes the engine-level coverage gap on the
// P3 no-raw-leak synthesis guard (review TQ-1). It exercises the engine wiring
// directly: the condition (SearchKnowledge ran this turn), the >=32-rune
// verbatim-content replacement, and the hardblock trace category.
func TestGuardSearchKnowledgeSynthesis(t *testing.T) {
	// A real-shaped runbook sentence, < 96 runes so the only leak needle is the
	// full sentence (the delimiter is 。 not ，). An answer echoing it verbatim leaks.
	chunkContent := "缩短上下文长度可以显著降低显存占用，把 max-model-len 设为略大于实际最大输入输出长度即可，过长会占用更多 KV cache。"
	hit := knowledge.RetrievalHit{Kept: true, Score: 90, Chunk: knowledge.KBChunk{ChunkID: "ext-gpu-oom-vllm-001", Content: chunkContent}}

	newEng := func() (*Engine, *[]observability.EngineHardBlockTrace) {
		eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}, &mockExecutor{}, nil)
		var traces []observability.EngineHardBlockTrace
		eng.SetHardBlockObserver(func(tr observability.EngineHardBlockTrace) { traces = append(traces, tr) })
		return eng, &traces
	}

	// 1. Leak: answer dumps the raw chunk sentence verbatim -> replaced + traced.
	eng, traces := newEng()
	eng.searchKnowledgeRanThisTurn = true
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{hit}
	got := eng.guardSearchKnowledgeSynthesis(chunkContent)
	assert.Equal(t, ragNoEvidenceReply, got, "raw >=32-rune chunk leak must be replaced")
	require.Len(t, *traces, 1)
	assert.Equal(t, "search_knowledge_raw_leak", (*traces)[0].Category)
	assert.True(t, (*traces)[0].Hit)

	// 2. Clean: a short paraphrase with only the short flag token passes unchanged.
	eng2, traces2 := newEng()
	eng2.searchKnowledgeRanThisTurn = true
	eng2.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{hit}
	clean := "把 max-model-len 调小一点即可省显存。"
	assert.Equal(t, clean, eng2.guardSearchKnowledgeSynthesis(clean), "paraphrase with only short tokens must pass")
	assert.Empty(t, *traces2, "no leak => no hardblock trace")

	// 3. Not-run: when SearchKnowledge did not run this turn, the guard is a no-op
	//    even on identical-to-leak content — this is the flag-off byte-identity path.
	eng3, traces3 := newEng()
	eng3.searchKnowledgeRanThisTurn = false
	assert.Equal(t, chunkContent, eng3.guardSearchKnowledgeSynthesis(chunkContent), "guard inert when SearchKnowledge did not run")
	assert.Empty(t, *traces3)
}
