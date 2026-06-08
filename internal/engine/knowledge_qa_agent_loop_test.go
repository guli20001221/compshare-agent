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

// knowledgeQAAgentLoopBillingChunk is a customer-safe billing FAQ chunk used by the
// agent-loop route tests. English content so a Chinese paraphrase answer can never
// trip the >=32-rune no-raw-leak guard (the cite arm is what these tests exercise).
func knowledgeQAAgentLoopBillingChunk() knowledge.KBChunk {
	return knowledge.KBChunk{
		ChunkID:    "faq-billing-001",
		KBVersion:  "kb.v1",
		SourceType: "runbook",
		Title:      "Billing after stop",
		Content:    "Stopped on-demand instances still charge for disks.",
		SourceURL:  "https://example.test/billing",
	}
}

// TestKnowledgeQAAgentLoop_RouteGate_ForcesSearchKnowledgeFirstHop is the Phase-1
// hinge test: with COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP on (and agentic SearchKnowledge
// on + a retriever wired), a knowledge_qa turn is routed into the shared ReAct loop
// with a FORCED SearchKnowledge first hop instead of the terminal-RAG route. It
// proves (a) the forced object tool_choice on the first LLM call, (b) the agent loop
// actually retrieves (SearchKnowledge ran on the engine retriever), (c) the distinct
// dispatched_knowledge_agent_loop route status with PlannedRuntimeForm=agent (so the
// runtime-form mismatch gate does not false-flag), and (d) turn-scoped cite-or-refuse
// parity (a properly [[chunk_id]]-cited synthesis is kept with markers stripped, NOT
// refused) — all WITHOUT the global grounded-validator flag.
func TestKnowledgeQAAgentLoop_RouteGate_ForcesSearchKnowledgeFirstHop(t *testing.T) {
	SetKnowledgeQAAgentLoopEnabled(true)
	defer SetKnowledgeQAAgentLoopEnabled(false)
	tools.SetAgenticSearchKnowledgeEnabled(true)
	defer tools.SetAgenticSearchKnowledgeEnabled(false)
	// Deliberately leave the global grounded-validator OFF to prove the agent-loop
	// route enforces cite-or-refuse turn-scoped on its own.
	SetGroundedAnswerValidatorEnabled(false)

	chunk := knowledgeQAAgentLoopBillingChunk()
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: knowledgeQAPlan(false)}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:   true,
		KBVersion: "kb.v1",
		Hits:      []knowledge.KBChunk{chunk},
		HitItems:  []knowledge.RetrievalHit{{Chunk: chunk, Score: 90, Kept: true}},
	}}}
	// Round 0: forced SearchKnowledge call. Round 1: a Chinese paraphrase that cites
	// the retrieved chunk_id — grounded, so the cite guard keeps it (markers stripped).
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{{
			ID:       "call-sk",
			Type:     openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: "SearchKnowledge", Arguments: `{"query":"why do stopped instances still bill"}`},
		}}},
		{Content: "停止的按量实例的磁盘仍会计费 [[faq-billing-001]]。"},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	var plannerTraces []observability.PlannerTrace
	eng.SetPlannerTraceObserver(func(tr observability.PlannerTrace) { plannerTraces = append(plannerTraces, tr) })
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "why do stopped instances still bill", noopStep)
	require.NoError(t, err)

	// (a) the first ReAct LLM call forced the SearchKnowledge object tool_choice.
	require.GreaterOrEqual(t, len(mock.calls), 1)
	tc, ok := mock.calls[0].ToolChoice.(openai.ToolChoice)
	require.True(t, ok, "first ReAct call must carry an object tool_choice, got %T", mock.calls[0].ToolChoice)
	assert.Equal(t, openai.ToolTypeFunction, tc.Type)
	assert.Equal(t, "SearchKnowledge", tc.Function.Name)
	assert.True(t, toolListContainsFunction(mock.calls[0].Tools, "SearchKnowledge"),
		"SearchKnowledge must be in the request tool list (agentic on => full knowledge_qa subset)")

	// (b) the agent loop actually retrieved on the engine retriever (not the terminal path).
	require.Len(t, retriever.calls, 1)
	assert.Equal(t, "why do stopped instances still bill", retriever.calls[0].question)
	assert.True(t, eng.searchKnowledgeRanThisTurn)
	assert.True(t, eng.knowledgeQAAgentLoopThisTurn, "the turn must be marked as agent-loop routed")

	// (c) distinct route status + planned form == actual (agent), so no mismatch.
	require.Len(t, plannerTraces, 1)
	assert.Equal(t, string(intent.RouteStatusDispatchedKnowledgeAgentLoop), plannerTraces[0].RouteStatus)
	assert.Equal(t, observability.RuntimeFormAgent, plannerTraces[0].PlannedRuntimeForm)
	assert.NotEqual(t, string(intent.RouteStatusDispatchedRetrieval), plannerTraces[0].RouteStatus,
		"agent-loop route must NOT report as terminal RAG")

	// (d) the cited synthesis is kept (cite-or-refuse parity active turn-scoped), markers stripped.
	assert.Contains(t, reply, "停止的按量实例的磁盘仍会计费")
	assert.NotContains(t, reply, "[[", "cite markers must be stripped for display")
	assert.NotEqual(t, ragNoEvidenceReply, reply, "a properly cited answer must NOT be refused")
}

// TestKnowledgeQAAgentLoop_FlagOff_TerminalRouteUnchanged proves the default-off
// byte-identity of the ROUTE: with the flag off (even with agentic on), a knowledge_qa
// turn still takes the deterministic terminal-RAG route (dispatched_retrieval, no tools
// exposed, no forced tool_choice) and is never marked as agent-loop routed.
func TestKnowledgeQAAgentLoop_FlagOff_TerminalRouteUnchanged(t *testing.T) {
	// Flag OFF (default). Agentic ON to prove the FLAG, not agentic, gates the route.
	tools.SetAgenticSearchKnowledgeEnabled(true)
	defer tools.SetAgenticSearchKnowledgeEnabled(false)

	chunk := knowledgeQAAgentLoopBillingChunk()
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: knowledgeQAPlan(false)}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:   true,
		KBVersion: "kb.v1",
		Hits:      []knowledge.KBChunk{chunk},
		HitItems:  []knowledge.RetrievalHit{{Chunk: chunk, Score: 90, Kept: true}},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "Stopped on-demand instances still charge for disks. [1]"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	var plannerTraces []observability.PlannerTrace
	eng.SetPlannerTraceObserver(func(tr observability.PlannerTrace) { plannerTraces = append(plannerTraces, tr) })
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
	eng.SetKnowledgeRetriever(retriever)

	_, err := eng.Chat(context.Background(), "why do stopped instances still bill", noopStep)
	require.NoError(t, err)

	assert.False(t, eng.knowledgeQAAgentLoopThisTurn, "flag off: turn must NOT be agent-loop routed")
	require.Len(t, mock.calls, 1)
	assert.Empty(t, mock.calls[0].Tools, "flag off: terminal RAG must not expose API tools")
	assert.Nil(t, mock.calls[0].ToolChoice, "flag off: terminal RAG must not force a tool")
	require.Len(t, plannerTraces, 1)
	assert.Equal(t, string(intent.RouteStatusDispatchedRetrieval), plannerTraces[0].RouteStatus)
}

// TestKnowledgeQAAgentLoop_InertWhenAgenticOff proves the 400-trap guard: with the
// flag ON but agentic SearchKnowledge OFF, the route gate is inert (the tool is not in
// the registry, so forcing it would 400) — the turn stays on the terminal route.
func TestKnowledgeQAAgentLoop_InertWhenAgenticOff(t *testing.T) {
	SetKnowledgeQAAgentLoopEnabled(true)
	defer SetKnowledgeQAAgentLoopEnabled(false)
	// Agentic OFF (default) => SearchKnowledge not visible => flag must be inert.

	chunk := knowledgeQAAgentLoopBillingChunk()
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: knowledgeQAPlan(false)}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:   true,
		KBVersion: "kb.v1",
		Hits:      []knowledge.KBChunk{chunk},
		HitItems:  []knowledge.RetrievalHit{{Chunk: chunk, Score: 90, Kept: true}},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "Stopped on-demand instances still charge for disks. [1]"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	var plannerTraces []observability.PlannerTrace
	eng.SetPlannerTraceObserver(func(tr observability.PlannerTrace) { plannerTraces = append(plannerTraces, tr) })
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
	eng.SetKnowledgeRetriever(retriever)

	_, err := eng.Chat(context.Background(), "why do stopped instances still bill", noopStep)
	require.NoError(t, err)

	assert.False(t, eng.knowledgeQAAgentLoopThisTurn, "agentic off: flag must be inert (stay on terminal route)")
	require.Len(t, mock.calls, 1)
	assert.Nil(t, mock.calls[0].ToolChoice, "agentic off: never force an absent tool")
	require.Len(t, plannerTraces, 1)
	assert.Equal(t, string(intent.RouteStatusDispatchedRetrieval), plannerTraces[0].RouteStatus)
}

// TestKnowledgeQAAgentLoop_CiteOrRefuseParity_WithoutGlobalValidator proves the
// turn-scoped cite-or-refuse coupling directly on guardSearchKnowledgeSynthesis: with
// the GLOBAL grounded-validator OFF but the turn marked as agent-loop routed, an
// uncited substantive answer is refused (search_knowledge_uncited) and a properly
// cited one is kept with markers stripped — exactly the terminal route's guarantee,
// preserved without flipping the global validator.
func TestKnowledgeQAAgentLoop_CiteOrRefuseParity_WithoutGlobalValidator(t *testing.T) {
	SetGroundedAnswerValidatorEnabled(false) // global validator OFF on purpose

	hit := knowledge.RetrievalHit{Kept: true, Score: 90, Chunk: knowledge.KBChunk{
		ChunkID: "ext-vllm-oom-001",
		Content: "把 max-model-len 设小一点即可显著降低显存占用，过长的上下文会占用更多 KV cache。",
	}}
	ledger := knowledge.EvidenceLedger{Items: []knowledge.EvidenceItem{{ChunkID: "ext-vllm-oom-001", Title: "vLLM OOM"}}}

	newEng := func() (*Engine, *[]observability.EngineHardBlockTrace) {
		eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}, &mockExecutor{}, nil)
		var traces []observability.EngineHardBlockTrace
		eng.SetHardBlockObserver(func(tr observability.EngineHardBlockTrace) { traces = append(traces, tr) })
		eng.searchKnowledgeRanThisTurn = true
		eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{hit}
		eng.searchKnowledgeLedgerThisTurn = ledger
		eng.knowledgeQAAgentLoopThisTurn = true // the only thing turning the cite arm on
		return eng, &traces
	}

	t.Run("uncited substantive answer refused (parity with terminal cite-or-refuse)", func(t *testing.T) {
		eng, traces := newEng()
		ans := "可以把 max-model-len 调小来省显存。"
		assert.Equal(t, ragNoEvidenceReply, eng.guardSearchKnowledgeSynthesis(ans))
		require.Len(t, *traces, 1)
		assert.Equal(t, "search_knowledge_uncited", (*traces)[0].Category)
	})

	t.Run("cited answer kept with markers stripped", func(t *testing.T) {
		eng, traces := newEng()
		got := eng.guardSearchKnowledgeSynthesis("可以把 max-model-len 调小来省显存 [[ext-vllm-oom-001]]。")
		assert.NotContains(t, got, "[[")
		assert.NotEqual(t, ragNoEvidenceReply, got)
		assert.Empty(t, *traces)
	})

	t.Run("inert when neither global validator nor agent-loop flag set", func(t *testing.T) {
		eng, traces := newEng()
		eng.knowledgeQAAgentLoopThisTurn = false // both off now
		ans := "可以把 max-model-len 调小来省显存。"
		assert.Equal(t, ans, eng.guardSearchKnowledgeSynthesis(ans), "neither gate on => uncited answer passes (byte-identical)")
		assert.Empty(t, *traces)
	})
}

// TestToolListContainsFunction unit-tests the 400-trap helper: present, absent, and
// the nil-Function guard.
func TestToolListContainsFunction(t *testing.T) {
	toolDefs := []openai.Tool{
		{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{Name: "DescribeCompShareInstance"}},
		{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{Name: "SearchKnowledge"}},
		{Type: openai.ToolTypeFunction, Function: nil}, // must not panic
	}
	assert.True(t, toolListContainsFunction(toolDefs, "SearchKnowledge"))
	assert.False(t, toolListContainsFunction(toolDefs, "GetCompShareInstanceMonitor"))
	assert.False(t, toolListContainsFunction(nil, "SearchKnowledge"))
}
