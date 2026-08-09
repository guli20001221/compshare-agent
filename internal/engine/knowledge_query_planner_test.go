package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKnowledgeQueryPlannerSkipsExtraCallWithoutConversation(t *testing.T) {
	mock := &mockLLM{}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.turnContextViewReady = true
	eng.turnContextViewThisTurn = AgentContext{CurrentQuestion: "关机后还收费吗"}

	got := eng.planKnowledgeQuery(context.Background(), "关机后还收费吗")

	assert.Equal(t, "关机后还收费吗", got.AnswerQuestion)
	assert.Equal(t, []string{"关机后还收费吗"}, got.SearchQueries)
	assert.Empty(t, mock.calls, "a standalone first-turn question must not pay for a rewrite call")
}

func TestKnowledgeQueryPlannerSeparatesAnswerQuestionFromSearchQueries(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: `{
		"answer_question":"按量实例关机后，哪些资源停止计费，哪些资源继续计费？",
		"search_queries":[
			"按量实例关机后 CPU GPU 内存计费规则",
			"关机后系统盘 数据盘 自制镜像计费规则"
		]
	}`}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.turnContextViewReady = true
	eng.turnContextViewThisTurn = AgentContext{
		CurrentQuestion: "那关机以后呢，还会继续收哪些费用？",
		RecentConversation: []ConversationPair{{
			User:      "磁盘空间是如何收费的？100GB 原始空间免费吗？",
			Assistant: "系统盘 100GB 内免费，超出部分及数据盘另行计费。",
		}},
	}

	got := eng.planKnowledgeQuery(context.Background(), "关机后磁盘收费")

	require.Len(t, mock.calls, 1)
	require.NotNil(t, mock.calls[0].ResponseFormat)
	assert.Equal(t, openai.ChatCompletionResponseFormatTypeJSONObject, mock.calls[0].ResponseFormat.Type)
	assert.Equal(t, openai.ChatMessageRoleSystem, mock.calls[0].Messages[0].Role,
		"the planner contract is internal control, never a fake user message")
	assert.Equal(t, "按量实例关机后，哪些资源停止计费，哪些资源继续计费？", got.AnswerQuestion)
	assert.Equal(t, []string{
		"按量实例关机后 CPU GPU 内存计费规则",
		"关机后系统盘 数据盘 自制镜像计费规则",
		"关机后磁盘收费",
	}, got.SearchQueries)
}

func TestKnowledgeQueryPlannerFailureFallsBackToAgentQuery(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "not json"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.turnContextViewReady = true
	eng.turnContextViewThisTurn = AgentContext{
		CurrentQuestion:    "浏览器里呢？",
		RecentConversation: []ConversationPair{{User: "远程桌面怎么配置音频？", Assistant: "使用 mstsc 配置。"}},
	}

	got := eng.planKnowledgeQuery(context.Background(), "浏览器 Windows 音频")

	assert.Equal(t, fallbackKnowledgeQueryPlan("浏览器 Windows 音频"), got)
	require.Len(t, mock.calls, 1)
}

// The JSON here is deliberately WELL-FORMED and parses cleanly — a truncated
// generation does not reliably produce invalid JSON (the model can be cut off
// right after closing the object), so this must be caught by the finish_reason
// check itself, not incidentally by a parse failure. It also proposes DIFFERENT
// queries than the fallback, so the assertion distinguishes "used the plan" from
// "fell back" rather than passing either way.
func TestKnowledgeQueryPlannerLengthStoppedResponseFallsBackToAgentQuery(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{
		Content:    `{"answer_question":"按量实例关机后收费规则","search_queries":["按量实例关机后收费规则"]}`,
		StopReason: "length",
	}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.turnContextViewReady = true
	eng.turnContextViewThisTurn = AgentContext{
		CurrentQuestion:    "浏览器里呢？",
		RecentConversation: []ConversationPair{{User: "远程桌面怎么配置音频？", Assistant: "使用 mstsc 配置。"}},
	}

	got := eng.planKnowledgeQuery(context.Background(), "浏览器 Windows 音频")

	assert.Equal(t, fallbackKnowledgeQueryPlan("浏览器 Windows 音频"), got,
		"a length-stopped planner response must not replace the Agent's own query")
	require.Len(t, mock.calls, 1)
}

func TestSearchKnowledgeUsesPlannedQueriesButKeepsOneAnswerQuestion(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: `{
		"answer_question":"浏览器通过 noVNC 打开 Windows 实例时是否支持音频？",
		"search_queries":["noVNC 浏览器 Windows 实例 音频支持","浏览器远程连接 音频限制"]
	}`}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{
		{Enabled: true, HybridMode: "qwen3_rrf", HitItems: []knowledge.RetrievalHit{{
			Kept: true, Score: 0.9,
			Chunk: knowledge.KBChunk{ChunkID: "novnc-audio", KBVersion: "kb.v1", Title: "noVNC", Content: "noVNC 不提供音频通道。"},
		}}},
		{Enabled: true, HybridMode: "qwen3_rrf", HitItems: []knowledge.RetrievalHit{{
			Kept: true, Score: 0.8,
			Chunk: knowledge.KBChunk{ChunkID: "browser-limit", KBVersion: "kb.v1", Title: "浏览器连接", Content: "浏览器访问与 mstsc 的能力不同。"},
		}}},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)
	eng.knowledgeQAAgentLoopThisTurn = true
	eng.turnContextViewReady = true
	eng.turnContextViewThisTurn = AgentContext{
		CurrentQuestion:    "如果是浏览器里打开的 Windows 实例呢？",
		RecentConversation: []ConversationPair{{User: "怎么配置远程桌面的音频？", Assistant: "可在 mstsc 中启用远程音频。"}},
	}

	out := eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "Windows 音频"}, noopStep)

	require.Len(t, retriever.calls, 3)
	assert.Equal(t, "noVNC 浏览器 Windows 实例 音频支持", retriever.calls[0].question)
	assert.Equal(t, "浏览器远程连接 音频限制", retriever.calls[1].question)
	// The Agent's own query is retrieved too, and last. Replacing it with the
	// planner's rewrite scored 27/37 delivered on the manual GT against 32/37 for
	// shipping the Agent's wording; retrieving both scored 33/37.
	assert.Equal(t, "Windows 音频", retriever.calls[2].question)
	assert.Equal(t, "浏览器通过 noVNC 打开 Windows 实例时是否支持音频？", eng.resolvedKnowledgeQuestionThisTurn)
	assert.Equal(t, eng.resolvedKnowledgeQuestionThisTurn, eng.searchKnowledgeLedgerThisTurn.Query)
	assert.Contains(t, out, "novnc-audio")
	assert.Contains(t, out, "browser-limit")
	// The original intent of this assertion — "retrievals are counted, not tool
	// envelopes" — is preserved, on the counter that now owns it. The inputs
	// above are untouched; only the field the count lives on moved, because one
	// counter used to serve both budgets and the wide plan below therefore ate
	// every later hop. Both halves are pinned so the split cannot silently
	// collapse back into a single counter.
	assert.Equal(t, 3, eng.searchKnowledgeQueriesThisTurn, "the retrieval budget counts actual retrievals, not tool envelopes")
	assert.Equal(t, 1, eng.searchKnowledgeCallsThisTurn, "the call budget counts agent decisions to search: this was one call")
}

func TestMultiTurnKnowledgeQueryPlanningIsWiredThroughTheProductionAgentLoop(t *testing.T) {
	chunk := knowledge.KBChunk{
		ChunkID:   "billing-after-stop",
		KBVersion: "kb.v1",
		Title:     "关机后的计费规则",
		Content:   "按量实例关机后计算资源停止计费，超出免费额度的磁盘仍继续计费。",
	}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:   true,
		KBVersion: chunk.KBVersion,
		HitItems: []knowledge.RetrievalHit{{
			Kept: true, Score: 0.9, Chunk: chunk,
		}},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{{
			ID:   "search",
			Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{
				Name:      "SearchKnowledge",
				Arguments: `{"query":"关机后还收什么费用"}`,
			},
		}}},
		{Content: `{
			"answer_question":"按量实例关机后还会收取哪些费用？",
			"search_queries":["按量实例关机后 计算资源 磁盘 计费规则"]
		}`},
		{Content: "关机后计算资源停止计费，超出免费额度的磁盘仍会计费[[billing-after-stop]]。"},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.RehydrateHistory([]HistoryMessage{
		{Role: openai.ChatMessageRoleUser, Content: "磁盘空间怎么收费，100GB 免费吗？"},
		{Role: openai.ChatMessageRoleAssistant, Content: "系统盘 100GB 内免费，超出部分及数据盘另行计费。"},
	})
	eng.SetKnowledgeRetriever(retriever)

	got, err := eng.Chat(context.Background(), "那关机以后呢，还会继续收哪些费用？", noopStep)
	require.NoError(t, err)
	assert.Equal(t, "关机后计算资源停止计费，超出免费额度的磁盘仍会计费。", got)
	require.Len(t, retriever.calls, 2)
	assert.Equal(t, "按量实例关机后 计算资源 磁盘 计费规则", retriever.calls[0].question)
	assert.Equal(t, "关机后还收什么费用", retriever.calls[1].question,
		"the Agent's own query is anchored last, never replaced by the planner")
	assert.Equal(t, "按量实例关机后还会收取哪些费用？", eng.resolvedKnowledgeQuestionThisTurn)
	require.Len(t, mock.calls, 3)
	require.NotNil(t, mock.calls[1].ResponseFormat)
	assert.Equal(t, openai.ChatMessageRoleSystem, mock.calls[1].Messages[0].Role,
		"the internal planner must never appear as another user turn")
}

// TestPlannerNeverDropsTheAgentsOwnQuery pins the property the 2026-08-09 arms
// bought (eval/reports/rag_retrieval_probe_2026-08-09.md §9): the planner may add
// a written-form rewrite, but it may not take away the query the Agent chose.
//
// Replacing that query is the arm that regressed — 27/37 delivered against 32/37 —
// and every case it broke carried an exact token an LLM rewrite is prone to smooth
// away ("226604 资源不足 创建实例报错" went from 0.994 to outside the candidate pool).
// A count assertion alone would not catch a regression that reorders the anchor to
// the front, which matters because retrievals are charged in order.
func TestPlannerNeverDropsTheAgentsOwnQuery(t *testing.T) {
	for _, tc := range []struct {
		name    string
		planned []string
		want    []string
	}{
		{
			name:    "rewrite is retrieved first, the Agent's query last",
			planned: []string{"实例关机后是否继续计费"},
			want:    []string{"实例关机后是否继续计费", "226604 资源不足"},
		},
		{
			name:    "a full-length plan cannot push the anchor out",
			planned: []string{"实例关机后是否继续计费", "按量付费实例关机计费规则"},
			want:    []string{"实例关机后是否继续计费", "按量付费实例关机计费规则", "226604 资源不足"},
		},
		{
			name:    "no duplicate when the planner already echoed it back",
			planned: []string{"226604 资源不足"},
			want:    []string{"226604 资源不足"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := validateKnowledgeQueryPlan(
				knowledgeQueryPlan{AnswerQuestion: "关机后还会计费吗", SearchQueries: tc.planned},
				fallbackKnowledgeQueryPlan("226604 资源不足"),
				maxKnowledgePlanQueries-1,
			)
			got := withAgentQueryAnchor(plan, "226604 资源不足")
			assert.Equal(t, tc.want, got.SearchQueries)
			assert.LessOrEqual(t, len(got.SearchQueries), maxKnowledgePlanQueries,
				"the anchor must not widen the per-turn retrieval budget")
		})
	}
}

// TestPlannerFallbackStillRetrievesTheAgentQuery guards the failure paths. Every
// early return in planKnowledgeQuery hands back fallbackKnowledgeQueryPlan, so a
// transport error, a truncated reply or a first turn must still search on the
// Agent's wording — the anchor is an addition to that, never a precondition for it.
func TestPlannerFallbackStillRetrievesTheAgentQuery(t *testing.T) {
	plan := fallbackKnowledgeQueryPlan("  226604 资源不足  ")
	assert.Equal(t, []string{"226604 资源不足"}, plan.SearchQueries)
	assert.Equal(t, "226604 资源不足", plan.AnswerQuestion)

	assert.Empty(t, fallbackKnowledgeQueryPlan("   ").SearchQueries)
	assert.Equal(t, []string{"改写后的完整问句"},
		withAgentQueryAnchor(knowledgeQueryPlan{SearchQueries: []string{"改写后的完整问句"}}, "").SearchQueries,
		"an empty Agent query must not append an empty retrieval")
}
