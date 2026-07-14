package engine

import (
	"context"
	"strings"
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

func knowledgeQAAgentLoopBillingVerdict() llm.ChatResponse {
	return llm.ChatResponse{Content: `{"supported":true,"claims":[{"answer_quote":"停止的按量实例的磁盘仍会计费","chunk_id":"faq-billing-001","evidence_quote":"Stopped on-demand instances still charge for disks"}],"unsupported":[]}`}
}

// TestKnowledgeQAAgentLoop_RouteGate_ForcesSearchKnowledgeFirstHop is the Phase-1
// hinge test: with COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP on (and agentic SearchKnowledge
// on + a retriever wired), a knowledge_qa turn is routed into the shared ReAct loop
// with a FORCED SearchKnowledge first hop instead of the terminal-RAG route. It
// proves (a) the forced object tool_choice on the first LLM call, (b) the agent loop
// actually retrieves (SearchKnowledge ran on the engine retriever), (c) the distinct
// dispatched_knowledge_agent_loop route status with PlannedExecutionPath=agent (so the
// runtime-form mismatch gate does not false-flag), and (d) turn-scoped cite-or-refuse
// parity (a properly [[chunk_id]]-cited synthesis is kept with markers stripped, NOT
// refused) — all WITHOUT the global grounded-validator flag.
func TestKnowledgeQAAgentLoop_RouteGate_ForcesSearchKnowledgeFirstHop(t *testing.T) {
	enableKnowledgeAnswerVerifier(t)
	SetKnowledgeQAAgentLoopEnabled(true)
	defer SetKnowledgeQAAgentLoopEnabled(false)
	tools.SetAgenticSearchKnowledgeEnabled(true)
	defer tools.SetAgenticSearchKnowledgeEnabled(false)
	// Deliberately leave the global grounded-validator OFF to prove the agent-loop
	// route enforces cite-or-refuse turn-scoped on its own.
	SetGroundedAnswerValidatorEnabled(false)

	chunk := knowledgeQAAgentLoopBillingChunk()
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{Plan: knowledgeQAPlan(false)}}}
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
		knowledgeQAAgentLoopBillingVerdict(),
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	var plannerTraces []observability.RouterTrace
	var retrievalTraces []observability.RetrievalTrace
	eng.SetPlannerTraceObserver(func(tr observability.RouterTrace) { plannerTraces = append(plannerTraces, tr) })
	eng.SetRetrievalTraceObserver(func(tr observability.RetrievalTrace) { retrievalTraces = append(retrievalTraces, tr) })
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
	assert.Equal(t, observability.ExecutionPathAgent, plannerTraces[0].PlannedExecutionPath)
	assert.NotEqual(t, string(intent.RouteStatusDispatchedRetrieval), plannerTraces[0].RouteStatus,
		"agent-loop route must NOT report as terminal RAG")

	// (d) the cited synthesis is kept (cite-or-refuse parity active turn-scoped), markers stripped.
	assert.Contains(t, reply, "停止的按量实例的磁盘仍会计费")
	assert.NotContains(t, reply, "[[", "cite markers must be stripped for display")
	assert.NotEqual(t, ragNoEvidenceReply, reply, "a properly cited answer must NOT be refused")
	require.NotEmpty(t, retrievalTraces)
	finalRetrieval := retrievalTraces[len(retrievalTraces)-1]
	assert.Equal(t, []string{"faq-billing-001"}, finalRetrieval.CitedChunkIDs)
	assert.Equal(t, []observability.RetrievalCitedRef{{RefID: "1", ChunkID: "faq-billing-001"}}, finalRetrieval.CitedRefs)
	require.Len(t, finalRetrieval.References, 1)
	assert.Equal(t, "1", finalRetrieval.References[0].RefID)
	assert.Equal(t, "faq-billing-001", finalRetrieval.References[0].ChunkID)
}

// A context-dependent follow-up does not need another retrieval when the prior
// completed answer already contains everything needed. This pins the real
// "粘贴呢" shape: the agent sees the previous answer, gives the correct direct
// continuation, and does not pay for or risk a lossy query rewrite.
func TestKnowledgeQAAgentLoop_ShortFollowupReusesSufficientConversationWithoutRAG(t *testing.T) {
	enableKnowledgeAnswerVerifier(t)
	SetKnowledgeQAAgentLoopEnabled(true)
	defer SetKnowledgeQAAgentLoopEnabled(false)
	tools.SetAgenticSearchKnowledgeEnabled(true)
	defer tools.SetAgenticSearchKnowledgeEnabled(false)

	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{Plan: knowledgeQAPlan(false)}}}
	retriever := &scriptedKnowledgeRetriever{}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "在 Windows Terminal 中直接按 Ctrl+Shift+V 粘贴，然后回车执行。"},
		{Content: `{"supported":true,"claims":[{"answer_quote":"在 Windows Terminal 中直接按 Ctrl+Shift+V 粘贴","chunk_id":"terminal-paste-001","evidence_quote":"使用 Ctrl+Shift+V 粘贴"},{"answer_quote":"然后回车执行","chunk_id":"terminal-paste-001","evidence_quote":"再按回车执行"}],"unsupported":[]}`},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent, VerifiedKnowledge: []VerifiedKnowledgeTurn{{
		Question: "Windows 终端里复制命令后怎么操作？",
		Answer:   "复制后回到 Windows Terminal，使用 Ctrl+Shift+V 粘贴，再按回车执行。",
		Evidence: knowledge.EvidenceLedger{Query: "Windows 终端里复制命令后怎么操作？", Items: []knowledge.EvidenceItem{{
			ChunkID: "terminal-paste-001", Title: "Windows Terminal 粘贴", Snippet: "使用 Ctrl+Shift+V 粘贴，再按回车执行。",
		}}},
	}}}, 1)
	eng.messages = append(eng.messages,
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Windows 终端里复制命令后怎么操作？"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "复制后回到 Windows Terminal，使用 Ctrl+Shift+V 粘贴，再按回车执行。"},
	)
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "粘贴呢", noopStep)
	require.NoError(t, err)
	assert.Contains(t, reply, "Ctrl+Shift+V")
	assert.Contains(t, reply, "回车")
	assert.Empty(t, retriever.calls, "sufficient committed context must not trigger redundant RAG")
	assert.False(t, eng.searchKnowledgeRanThisTurn)

	require.Len(t, mock.calls, 2, "direct continuation uses one bounded provenance check and no retrieval")
	first := mock.calls[0]
	assert.Nil(t, first.ToolChoice, "a complete prior turn lets the agent decide whether retrieval is needed")
	joined := make([]string, 0, len(first.Messages))
	for _, message := range first.Messages {
		joined = append(joined, message.Content)
	}
	visible := strings.Join(joined, "\n")
	assert.Contains(t, visible, "Windows 终端里复制命令后怎么操作？")
	assert.Contains(t, visible, "使用 Ctrl+Shift+V 粘贴")
	assert.Contains(t, visible, "粘贴呢")
	assert.Contains(t, visible, "完整对话")
	assert.Contains(t, visible, "可以直接回答")
	assert.Contains(t, visible, "需要新事实")
	assert.Contains(t, visible, "脱离上文")
	assert.NotEqual(t, ragNoEvidenceReply, reply)
}

// When prior conversation does not contain the missing fact, the same agent
// elects to search and owns the standalone query. The previous answer can
// resolve the query, but it never becomes retrieval evidence.
func TestKnowledgeQAAgentLoop_AgentFormulatesQueryWhenConversationIsInsufficient(t *testing.T) {
	enableKnowledgeAnswerVerifier(t)
	SetKnowledgeQAAgentLoopEnabled(true)
	defer SetKnowledgeQAAgentLoopEnabled(false)
	tools.SetAgenticSearchKnowledgeEnabled(true)
	defer tools.SetAgenticSearchKnowledgeEnabled(false)

	chunk := knowledge.KBChunk{
		ChunkID:    "terminal-paste-001",
		KBVersion:  "kb.v1",
		SourceType: "runbook",
		Title:      "Windows 终端粘贴",
		Content:    "在 Windows Terminal 中可以使用 Ctrl+Shift+V 粘贴。",
	}
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{Plan: knowledgeQAPlan(false)}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:   true,
		KBVersion: "kb.v1",
		Hits:      []knowledge.KBChunk{chunk},
		HitItems:  []knowledge.RetrievalHit{{Chunk: chunk, Score: 90, Kept: true}},
	}}}
	resolved := "Windows Terminal 中如何粘贴文本"
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{{
			ID:       "call-sk",
			Type:     openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: "SearchKnowledge", Arguments: `{"query":"` + resolved + `"}`},
		}}},
		{Content: "在 Windows Terminal 中可使用 Ctrl+Shift+V 粘贴 [[terminal-paste-001]]。"},
		{Content: `{"supported":true,"claims":[{"answer_quote":"在 Windows Terminal 中可使用 Ctrl+Shift+V 粘贴","chunk_id":"terminal-paste-001","evidence_quote":"在 Windows Terminal 中可以使用 Ctrl+Shift+V 粘贴"}],"unsupported":[]}`},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	eng.messages = append(eng.messages,
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "Windows 终端里复制命令后怎么操作？"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "请回到终端继续操作。"},
	)
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "粘贴呢", noopStep)
	require.NoError(t, err)
	assert.Contains(t, reply, "Ctrl+Shift+V")
	require.Len(t, retriever.calls, 1)
	assert.Equal(t, resolved, retriever.calls[0].question)
	assert.Equal(t, resolved, eng.resolvedKnowledgeQuestionThisTurn)
	assert.Equal(t, resolved, eng.searchKnowledgeLedgerThisTurn.Query)
	require.NotEmpty(t, mock.calls)
	assert.NotNil(t, mock.calls[0].ToolChoice, "without verified prior provenance the first search remains mandatory")

	var searchTool *openai.FunctionDefinition
	for _, tool := range mock.calls[0].Tools {
		if tool.Function != nil && tool.Function.Name == "SearchKnowledge" {
			searchTool = tool.Function
			break
		}
	}
	require.NotNil(t, searchTool)
	assert.Equal(t, "检索平台与第三方工具运维知识，返回带 chunk_id 的证据条目。", searchTool.Description)
	parameters, ok := searchTool.Parameters.(map[string]any)
	require.True(t, ok)
	properties, ok := parameters["properties"].(map[string]any)
	require.True(t, ok)
	querySchema, ok := properties["query"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, querySchema["description"], "独立理解")
	require.GreaterOrEqual(t, len(mock.calls), 3)
	verifierPayload := mock.calls[2].Messages[1].Content
	assert.NotContains(t, verifierPayload, "请回到终端继续操作",
		"the previous assistant answer may resolve the query but must never become retrieval evidence")
	assert.Contains(t, verifierPayload, `"resolved_question":"`+resolved+`"`)
}

// An uncited answer is accepted when the semantic verifier proves every claim.
// Citation punctuation is no longer the recovery target.
func TestKnowledgeQAAgentLoop_SemanticGateAcceptsUncitedGroundedSynthesis(t *testing.T) {
	enableKnowledgeAnswerVerifier(t)
	SetKnowledgeQAAgentLoopEnabled(true)
	defer SetKnowledgeQAAgentLoopEnabled(false)
	tools.SetAgenticSearchKnowledgeEnabled(true)
	defer tools.SetAgenticSearchKnowledgeEnabled(false)
	SetGroundedAnswerValidatorEnabled(false)

	chunk := knowledgeQAAgentLoopBillingChunk()
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{Plan: knowledgeQAPlan(false)}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled: true, KBVersion: "kb.v1",
		Hits:     []knowledge.KBChunk{chunk},
		HitItems: []knowledge.RetrievalHit{{Chunk: chunk, Score: 90, Kept: true}},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{{ID: "c1", Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: "SearchKnowledge", Arguments: `{"query":"billing"}`}}}},
		{Content: "停止的按量实例的磁盘仍会计费。"},         // uncited, but semantically grounded
		knowledgeQAAgentLoopBillingVerdict(), // the unified semantic gate accepts it
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "why do stopped instances still bill", noopStep)
	require.NoError(t, err)
	assert.Contains(t, reply, "停止的按量实例的磁盘仍会计费")
	assert.NotContains(t, reply, "[[", "display answer must have markers stripped")
	assert.NotEqual(t, ragNoEvidenceReply, reply)
	assert.GreaterOrEqual(t, len(mock.calls), 3, "expected tool-call + uncited synthesis + semantic verification")
}

// A failed semantic verdict gets at most one proof-carrying repair. If repair
// also fails, the reply states the real failure instead of claiming no evidence.
func TestKnowledgeQAAgentLoop_FailedSemanticRepairKeepsHonestRefusal(t *testing.T) {
	enableDisciplinedKnowledgeRepair(t)
	enableKnowledgeAnswerVerifier(t)
	SetKnowledgeQAAgentLoopEnabled(true)
	defer SetKnowledgeQAAgentLoopEnabled(false)
	tools.SetAgenticSearchKnowledgeEnabled(true)
	defer tools.SetAgenticSearchKnowledgeEnabled(false)
	SetGroundedAnswerValidatorEnabled(false)

	chunk := knowledgeQAAgentLoopBillingChunk()
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{Plan: knowledgeQAPlan(false)}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled: true, KBVersion: "kb.v1",
		Hits:     []knowledge.KBChunk{chunk},
		HitItems: []knowledge.RetrievalHit{{Chunk: chunk, Score: 90, Kept: true}},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{{ID: "c1", Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: "SearchKnowledge", Arguments: `{"query":"billing"}`}}}},
		{Content: "停止的按量实例的磁盘仍会计费。"},
		{Content: `{"supported":false,"claims":[],"unsupported":["无法确认"]}`},
		{Content: `{"answer":"","supported":false,"claims":[],"unsupported":["证据不足"]}`},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "why do stopped instances still bill", noopStep)
	require.NoError(t, err)
	assert.Equal(t, ragUngroundableReply, reply, "evidence existed, so the refusal must not falsely claim no coverage")
}

// TestKnowledgeQAAgentLoop_ForcedHopRetryRecoversMisfire proves the forced-hop retry:
// when flash ignores the forced SearchKnowledge object tool_choice at round 0 and
// answers directly (no tool call), the engine re-forces the first hop once; the retry
// fires SearchKnowledge and the turn proceeds to a grounded answer instead of the
// round-0 cited-gate refusal.
func TestKnowledgeQAAgentLoop_ForcedHopRetryRecoversMisfire(t *testing.T) {
	enableKnowledgeAnswerVerifier(t)
	SetKnowledgeQAAgentLoopEnabled(true)
	defer SetKnowledgeQAAgentLoopEnabled(false)
	tools.SetAgenticSearchKnowledgeEnabled(true)
	defer tools.SetAgenticSearchKnowledgeEnabled(false)
	SetGroundedAnswerValidatorEnabled(false)

	chunk := knowledgeQAAgentLoopBillingChunk()
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{Plan: knowledgeQAPlan(false)}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled: true, KBVersion: "kb.v1",
		Hits:     []knowledge.KBChunk{chunk},
		HitItems: []knowledge.RetrievalHit{{Chunk: chunk, Score: 90, Kept: true}},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "我直接回答，没有调用工具。"}, // round0: forced hop MISFIRED (no tool call)
		{ToolCalls: []openai.ToolCall{{ID: "c1", Type: openai.ToolTypeFunction, // forced-hop retry fires
			Function: openai.FunctionCall{Name: "SearchKnowledge", Arguments: `{"query":"billing"}`}}}},
		{Content: "停止的按量实例的磁盘仍会计费 [[faq-billing-001]]。"}, // round1 cited
		knowledgeQAAgentLoopBillingVerdict(),
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "why do stopped instances still bill", noopStep)
	require.NoError(t, err)
	assert.True(t, eng.searchKnowledgeRanThisTurn, "forced-hop retry must recover the misfire and fire SearchKnowledge")
	require.Len(t, retriever.calls, 1)
	assert.Contains(t, reply, "停止的按量实例的磁盘仍会计费")
	assert.NotEqual(t, ragNoEvidenceReply, reply)
	assert.GreaterOrEqual(t, len(mock.calls), 3, "round0 misfire + forced-hop retry + final synthesis")
}

// A prior turn makes retrieval optional, but it does not let the model claim
// that the KB has no answer without checking. That negative claim triggers one
// forced search; a substantive context answer would have been returned directly.
func TestKnowledgeQAAgentLoop_ContextAwareUnsearchedRefusalTriggersOneSearch(t *testing.T) {
	enableKnowledgeAnswerVerifier(t)
	SetKnowledgeQAAgentLoopEnabled(true)
	defer SetKnowledgeQAAgentLoopEnabled(false)
	tools.SetAgenticSearchKnowledgeEnabled(true)
	defer tools.SetAgenticSearchKnowledgeEnabled(false)

	chunk := knowledgeQAAgentLoopBillingChunk()
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{Plan: knowledgeQAPlan(false)}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled: true, KBVersion: "kb.v1",
		Hits:     []knowledge.KBChunk{chunk},
		HitItems: []knowledge.RetrievalHit{{Chunk: chunk, Score: 90, Kept: true}},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: ragNoEvidenceReply},
		{ToolCalls: []openai.ToolCall{{ID: "search", Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: "SearchKnowledge", Arguments: `{"query":"stopped instance disk billing"}`}}}},
		{Content: "停止的按量实例的磁盘仍会计费。"},
		knowledgeQAAgentLoopBillingVerdict(),
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent, VerifiedKnowledge: []VerifiedKnowledgeTurn{{
		Question: "按量实例关机后还收费吗？",
		Answer:   "CPU 和 GPU 会停止计费；其他保留资源需另行确认。",
		Evidence: knowledge.EvidenceLedger{Query: "按量实例关机后还收费吗？", Items: []knowledge.EvidenceItem{{
			ChunkID: "prior-billing-001", Title: "关机计费", Snippet: "关机后 CPU 和 GPU 停止计费，保留资源另行计费。",
		}}},
	}}}, 1)
	eng.messages = append(eng.messages,
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "按量实例关机后还收费吗？"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "CPU 和 GPU 会停止计费；其他保留资源需另行确认。"},
	)
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "磁盘呢？", noopStep)
	require.NoError(t, err)
	assert.Contains(t, reply, "磁盘仍会计费")
	require.Len(t, retriever.calls, 1)
	require.GreaterOrEqual(t, len(mock.calls), 4)
	assert.Nil(t, mock.calls[0].ToolChoice, "prior conversation makes the initial decision agentic")
	forced, ok := mock.calls[1].ToolChoice.(openai.ToolChoice)
	require.True(t, ok, "an unsearched no-coverage claim must trigger a forced check")
	assert.Equal(t, "SearchKnowledge", forced.Function.Name)
}

// Production enables the flash route correction, agent loop and semantic
// verifier together. A flash-corrected product-fact turn must enter the common
// SearchKnowledge loop instead of escaping through the legacy terminal RAG exit.
func TestKnowledgeQAAgentLoop_FlashGuardUsesUnifiedVerifiedExit(t *testing.T) {
	enableKnowledgeAnswerVerifier(t)
	previousLoop := KnowledgeQAAgentLoopEnabled()
	SetKnowledgeQAAgentLoopEnabled(true)
	t.Cleanup(func() { SetKnowledgeQAAgentLoopEnabled(previousLoop) })
	previousAgentic := tools.AgenticSearchKnowledgeEnabled()
	tools.SetAgenticSearchKnowledgeEnabled(true)
	t.Cleanup(func() { tools.SetAgenticSearchKnowledgeEnabled(previousAgentic) })
	previousFlash := FlashKnowledgeRouteGuardEnabled()
	SetFlashKnowledgeRouteGuardEnabled(true)
	t.Cleanup(func() { SetFlashKnowledgeRouteGuardEnabled(previousFlash) })

	chunk := knowledgeQAAgentLoopBillingChunk()
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{Plan: intent.IntentRoute{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentStockAvailability,
		Confidence:    0.9,
	}}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled: true, KBVersion: "kb.v1",
		Hits:     []knowledge.KBChunk{chunk},
		HitItems: []knowledge.RetrievalHit{{Chunk: chunk, Score: 90, Kept: true}},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{{ID: "search", Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: "SearchKnowledge", Arguments: `{"query":"stopped instance disk billing"}`}}}},
		{Content: "停止的按量实例的磁盘仍会计费。"},
		knowledgeQAAgentLoopBillingVerdict(),
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "关机后磁盘还收费吗", noopStep)
	require.NoError(t, err)
	assert.Contains(t, reply, "磁盘仍会计费")
	require.Len(t, retriever.calls, 1)
	require.Len(t, mock.calls, 3, "tool call, draft and semantic verifier must all run")
	assert.True(t, toolListContainsFunction(mock.calls[0].Tools, "SearchKnowledge"))
	assert.Contains(t, mock.calls[2].Messages[0].Content, "知识答案事实核查员")
	assert.True(t, eng.knowledgeQAAgentLoopThisTurn)
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

// TestToolListWithoutFunction unit-tests the cap helper: it removes the named tool,
// keeps the others, tolerates a nil Function entry, and never mutates the input.
func TestToolListWithoutFunction(t *testing.T) {
	in := []openai.Tool{
		{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{Name: "DescribeCompShareInstance"}},
		{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{Name: "SearchKnowledge"}},
		{Type: openai.ToolTypeFunction, Function: nil}, // must not panic
	}
	out := toolListWithoutFunction(in, "SearchKnowledge")
	assert.False(t, toolListContainsFunction(out, "SearchKnowledge"), "named tool removed")
	assert.True(t, toolListContainsFunction(out, "DescribeCompShareInstance"), "other tools kept")
	assert.Len(t, out, 2)
	assert.True(t, toolListContainsFunction(in, "SearchKnowledge"), "input slice must not be mutated")
}

// TestKnowledgeQAAgentLoop_SearchCapWithdrawsToolAndAnswers proves the corpus-gap
// thrash fix: when the model keeps re-calling SearchKnowledge (the real-world trigger
// is weak evidence on a query the corpus doesn't cover — here every retrieval is weak
// and dropped to an empty ledger), the loop withdraws SearchKnowledge after
// maxSearchKnowledgeCallsPerTurn calls and injects the honest-answer nudge, so the
// turn settles on a final reply instead of re-searching until the token budget trips
// (the D7 "Agent Plan" bug: 9 SearchKnowledge calls → 95K prompt tokens → bare
// "请简化问题"). WHY it matters: an uncovered question must yield a fast honest
// "no specific docs", never an opaque token-exhaustion message.
func TestKnowledgeQAAgentLoop_SearchCapWithdrawsToolAndAnswers(t *testing.T) {
	SetKnowledgeQAAgentLoopEnabled(true)
	defer SetKnowledgeQAAgentLoopEnabled(false)
	tools.SetAgenticSearchKnowledgeEnabled(true)
	defer tools.SetAgenticSearchKnowledgeEnabled(false)
	SetGroundedAnswerValidatorEnabled(false)

	// Weak hit (score 0.1 < weakEvidenceSemanticThreshold 0.5 on qwen3_rrf) → dropped
	// to an empty ledger, exactly like a corpus-gap query. One scripted result; the
	// retriever returns Empty for the remaining calls (same empty-ledger effect).
	weak := knowledge.RetrievalResult{
		Enabled: true, KBVersion: "kb.v1", HybridMode: "qwen3_rrf",
		HitItems: []knowledge.RetrievalHit{{Chunk: knowledge.KBChunk{ChunkID: "irrelevant-001", Content: "unrelated"}, Score: 0.1, Kept: true}},
	}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{weak}}
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{Plan: knowledgeQAPlan(false)}}}

	// The model keeps calling SearchKnowledge (the thrash). Script one call per round
	// up to the cap, then a final honest answer once the tool is withdrawn.
	skCall := llm.ChatResponse{ToolCalls: []openai.ToolCall{{
		ID: "sk", Type: openai.ToolTypeFunction,
		Function: openai.FunctionCall{Name: "SearchKnowledge", Arguments: `{"query":"agent plan 个人 团队"}`},
	}}}
	finalAnswer := "暂无该主题的专项文档，建议查阅优云控制台或官网帮助。"
	mock := &mockLLM{responses: []llm.ChatResponse{
		skCall, skCall, skCall, skCall, skCall, // 5 = maxSearchKnowledgeCallsPerTurn
		{Content: finalAnswer}, // round after the cap: tool withdrawn → final reply
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "Agent Plan 个人版和团队版有什么区别", noopStep)
	require.NoError(t, err)

	// SearchKnowledge ran exactly the cap, NOT all maxReActRounds — the thrash is bounded.
	require.Len(t, retriever.calls, maxSearchKnowledgeCallsPerTurn,
		"SearchKnowledge must be capped at maxSearchKnowledgeCallsPerTurn, not loop to maxReActRounds")
	require.GreaterOrEqual(t, len(mock.calls), maxSearchKnowledgeCallsPerTurn+1)

	// The tool is offered up to the cap, then withdrawn on the next round.
	assert.True(t, toolListContainsFunction(mock.calls[maxSearchKnowledgeCallsPerTurn-1].Tools, "SearchKnowledge"),
		"SearchKnowledge still offered on the cap-th call")
	capCall := mock.calls[maxSearchKnowledgeCallsPerTurn]
	assert.False(t, toolListContainsFunction(capCall.Tools, "SearchKnowledge"),
		"SearchKnowledge withdrawn once the cap is hit")
	// The honest-answer nudge is injected alongside the withdrawal.
	foundNote := false
	for _, m := range capCall.Messages {
		if m.Content == knowledgeQASearchCapNote {
			foundNote = true
		}
	}
	assert.True(t, foundNote, "the honest-answer nudge must be injected when the tool is withdrawn")

	// With an empty evidence ledger the final answer cannot be certified, even if
	// the model writes a plausible general fallback.
	assert.Equal(t, ragNoEvidenceReply, reply)
	assert.NotEqual(t, tokenBudgetExceededMessage, reply, "capped thrash must not exhaust the token budget")
}
