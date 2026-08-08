package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// planningEngineWithConversation builds an engine whose turn already carries
// prior conversation, which is the ONLY condition under which
// planKnowledgeQuery actually calls the model (see knowledge_query_planner.go:
// it returns the single-query fallback when RecentConversation is empty).
//
// This is why the sibling budget test in search_knowledge_test.go cannot catch
// the defect below: its fixture is a first turn, so every plan holds exactly one
// query and the per-query counter is indistinguishable from a per-call counter.
func planningEngineWithConversation(t *testing.T, planJSON string, results []knowledge.RetrievalResult) (*Engine, *scriptedKnowledgeRetriever) {
	t.Helper()
	retriever := &scriptedKnowledgeRetriever{results: results}
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: planJSON}}}, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)
	eng.knowledgeQAAgentLoopThisTurn = true
	eng.turnContextViewThisTurn = AgentContext{
		CurrentQuestion:    "关机后还收什么费用",
		RecentConversation: []ConversationPair{{User: "4090 一个月多少钱", Assistant: "按量计费约 ..."}},
	}
	eng.turnContextViewReady = true
	return eng, retriever
}

func twoHitResults() []knowledge.RetrievalResult {
	hit := func(id string) knowledge.RetrievalResult {
		return knowledge.RetrievalResult{
			Enabled:   true,
			KBVersion: "kb.v1",
			HitItems: []knowledge.RetrievalHit{{
				Kept:  true,
				Score: 90,
				Chunk: knowledge.KBChunk{ChunkID: id, KBVersion: "kb.v1", Title: id, Content: "evidence " + id},
			}},
		}
	}
	return []knowledge.RetrievalResult{hit("chunk-1"), hit("chunk-2"), hit("chunk-3"), hit("chunk-4")}
}

// TestSearchKnowledgeBudgetCountsCallsNotPlannedQueries pins the contract that
// maxSearchKnowledgeCallsPerTurn already documents in its own comment:
//
//	"One resolved query is normally sufficient; a second permits a genuine
//	 follow-up angle without allowing search thrash."
//
// The budget exists to stop the agent from re-searching round after round. It
// is NOT a budget on how many phrasings one resolved question fans out into —
// that fan-out happens inside a single call, costs one round, and is exactly
// what the planner was built to produce.
//
// Conflating the two makes the documented follow-up hop unreachable: a planner
// that emits two queries spends the whole turn budget on the FIRST call, and
// the engine then withdraws SearchKnowledge from the tool window (engine.go,
// "Once the bounded search budget is exhausted, remove the capability"). The
// agent loses multi-hop retrieval precisely on the multi-turn questions the
// planner was added to serve.
func TestSearchKnowledgeBudgetCountsCallsNotPlannedQueries(t *testing.T) {
	eng, retriever := planningEngineWithConversation(t,
		`{"answer_question":"实例关机后还会产生哪些费用","search_queries":["关机后计费规则","数据盘关机是否计费"]}`,
		twoHitResults())

	first := eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "关机后还收什么"}, noopStep)

	// The fan-out itself is desired: both planned angles were retrieved.
	require.Len(t, retriever.calls, 2, "both planned queries must be retrieved inside the one call")
	assert.NotContains(t, first, `"search_limit_reached":true`)

	// ...but it is ONE SearchKnowledge call, so it must cost ONE unit of the
	// per-turn call budget, leaving the documented follow-up hop available.
	assert.Equal(t, 1, eng.searchKnowledgeCallsThisTurn,
		"one SearchKnowledge call must consume one unit of the per-turn call budget, not one per planned query")

	// The condition the ReAct loop uses to withdraw the tool must NOT be true
	// after a single call, or the agent can never search a second time.
	assert.Less(t, eng.searchKnowledgeCallsThisTurn, maxSearchKnowledgeCallsPerTurn,
		"a single call must not exhaust the turn budget; the follow-up hop is part of the documented contract")

	second := eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "数据盘呢"}, noopStep)
	assert.NotContains(t, second, `"search_limit_reached":true`,
		"the second hop is the whole point of a budget of two")
}

// TestSearchKnowledgePlannedQueriesAreNeverSilentlyDropped covers the smaller
// half of the same defect: the planner prompt asks for up to
// maxKnowledgePlanQueries queries, but the per-query budget truncates the plan
// mid-loop, so a planned angle is dropped with nothing surfaced to the model or
// the trace beyond a reduced count.
func TestSearchKnowledgePlannedQueriesAreNeverSilentlyDropped(t *testing.T) {
	eng, retriever := planningEngineWithConversation(t,
		`{"answer_question":"重装系统会影响什么","search_queries":["重装系统盘会不会清空","重装数据盘是否保留","重装后需要重装驱动吗"]}`,
		twoHitResults())

	_ = eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "重装会影响什么"}, noopStep)

	require.Len(t, retriever.calls, 3,
		"every query the planner emitted must be retrieved; a plan that is generated and then truncated is a contract the code does not keep")
}
