package engine

import (
	"context"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"

	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/llm"
)

// capturingLLM records each request it receives and replays a scripted response
// per round, so a structural test can assert the first-decision tool window,
// tool_choice, and the round-2 window shape.
type capturingLLM struct {
	reqs  []llm.ChatRequest
	resps []*llm.ChatResponse
}

func (m *capturingLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	m.reqs = append(m.reqs, req)
	i := len(m.reqs) - 1
	if i < len(m.resps) {
		return m.resps[i], nil
	}
	return &llm.ChatResponse{Content: "（脚本用尽）"}, nil
}

func fdContains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func windowNames(tools []openai.Tool) []string {
	var out []string
	for _, t := range tools {
		if t.Function != nil {
			out = append(out, t.Function.Name)
		}
	}
	return out
}

func windowHasReadOrRAG(tools []openai.Tool) bool {
	for _, n := range windowNames(tools) {
		if strings.HasPrefix(n, "ReadCapability_") || n == "SearchKnowledge" {
			return true
		}
	}
	return false
}

func windowHasProposal(tools []openai.Tool) bool {
	for _, n := range windowNames(tools) {
		if isProposalToolName(n) {
			return true
		}
	}
	return false
}

func withForcedFirstDecision(t *testing.T) {
	t.Helper()
	SetForcedFirstDecisionEnabled(true)
	t.Cleanup(func() { SetForcedFirstDecisionEnabled(false) })
}

// GATE: the forced first-decision window contains only Request* proposal tools +
// ContinueWithoutWrite, is sent with tool_choice=required, and carries no read /
// RAG tool — the structural half of "no read/RAG before the first decision".
func TestFirstDecisionWindowIsWriteOrContinueWithRequired(t *testing.T) {
	withForcedFirstDecision(t)
	llmc := &capturingLLM{resps: []*llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("c1", continueWithoutWriteName, "{}")}},
		{Content: "这是最终回答。"},
	}}
	eng := NewWithDeps(llmc, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 0)

	_, err := eng.Chat(context.Background(), "怎么创建一台实例？", noopStep)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(llmc.reqs) < 1 {
		t.Fatalf("expected at least one LLM request")
	}
	first := llmc.reqs[0]
	if first.ToolChoice != "required" {
		t.Errorf("first-decision ToolChoice = %v, want \"required\"", first.ToolChoice)
	}
	if windowHasReadOrRAG(first.Tools) {
		t.Errorf("first-decision window must contain NO read/RAG tool; got %v", windowNames(first.Tools))
	}
	if !windowHasProposal(first.Tools) {
		t.Errorf("first-decision window must contain Request* proposal tools; got %v", windowNames(first.Tools))
	}
	names := windowNames(first.Tools)
	if !fdContains(names, continueWithoutWriteName) {
		t.Errorf("first-decision window must contain %s; got %v", continueWithoutWriteName, names)
	}
	if eng.firstDecisionOutcomeThisTurn != firstDecisionContinue {
		t.Errorf("outcome = %q, want %q", eng.firstDecisionOutcomeThisTurn, firstDecisionContinue)
	}
}

// GATE: after continue-without-write, the rest of the turn has NO Request* tool
// (and no ContinueWithoutWrite) — a write proposal can only be the first decision.
func TestFirstDecisionContinueStripsProposalToolsAfterward(t *testing.T) {
	withForcedFirstDecision(t)
	llmc := &capturingLLM{resps: []*llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("c1", continueWithoutWriteName, "{}")}},
		{Content: "读能力可用，写工具已移除。"},
	}}
	eng := NewWithDeps(llmc, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 0)

	if _, err := eng.Chat(context.Background(), "4090 现在有库存吗", noopStep); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(llmc.reqs) < 2 {
		t.Fatalf("expected a second round after continue; got %d requests", len(llmc.reqs))
	}
	second := llmc.reqs[1]
	if windowHasProposal(second.Tools) {
		t.Errorf("post-continue window must contain NO Request* tool; got %v", windowNames(second.Tools))
	}
	if fdContains(windowNames(second.Tools), continueWithoutWriteName) {
		t.Errorf("post-continue window must not re-offer %s", continueWithoutWriteName)
	}
	if !windowHasReadOrRAG(second.Tools) {
		t.Errorf("post-continue window should expose read/RAG tools; got %v", windowNames(second.Tools))
	}
}

// GATE: a Request* first choice is classified as a write proposal (falls through
// to the Resolver path) and closes the write window; no read preceded it.
func TestFirstDecisionRequestClassifiedAndClosesWindow(t *testing.T) {
	withForcedFirstDecision(t)
	llmc := &capturingLLM{resps: []*llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("r1", "RequestStopInstance", `{"UHostId":"uhost-x"}`)}},
		{Content: "已处理。"},
	}}
	eng := NewWithDeps(llmc, &mockExecutor{}, func(string, map[string]any) bool { return false })
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 0)

	if _, err := eng.Chat(context.Background(), "帮我关掉 uhost-x", noopStep); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := eng.firstDecisionOutcomeThisTurn; got != "request:RequestStopInstance" {
		t.Errorf("outcome = %q, want request:RequestStopInstance", got)
	}
	// GATE (Request → immediately Resolver): the seeded proposal runs straight
	// through the action-proposal resolver, not a prose back-and-forth.
	if !eng.actionProposalRanThisTurn {
		t.Errorf("Request first-decision must dispatch straight to the action-proposal resolver")
	}
	if !eng.writeWindowClosedThisTurn {
		t.Errorf("write window must be closed after the first-decision write proposal")
	}
	if windowHasReadOrRAG(llmc.reqs[0].Tools) {
		t.Errorf("no read/RAG may precede the write proposal; round-0 window = %v", windowNames(llmc.reqs[0].Tools))
	}
}

// GATE: more than one tool call on the forced first decision fails closed.
func TestFirstDecisionMultipleToolCallsFailClosed(t *testing.T) {
	withForcedFirstDecision(t)
	llmc := &capturingLLM{resps: []*llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("a", "RequestStopInstance", `{"UHostId":"uhost-x"}`),
			toolCall("b", continueWithoutWriteName, "{}"),
		}},
	}}
	eng := NewWithDeps(llmc, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 0)

	reply, err := eng.Chat(context.Background(), "帮我关掉 uhost-x", noopStep)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if eng.firstDecisionOutcomeThisTurn != firstDecisionFailMulti {
		t.Errorf("outcome = %q, want %q", eng.firstDecisionOutcomeThisTurn, firstDecisionFailMulti)
	}
	if reply != firstDecisionFailReply {
		t.Errorf("reply = %q, want the fail-closed refusal", reply)
	}
}

// GATE: an unexpected (non-Request*, non-Continue) tool on the first decision
// fails closed rather than executing.
func TestFirstDecisionUnknownToolFailsClosed(t *testing.T) {
	withForcedFirstDecision(t)
	llmc := &capturingLLM{resps: []*llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("x", "ReadCapability_stock_availability", "{}")}},
	}}
	eng := NewWithDeps(llmc, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 0)

	if _, err := eng.Chat(context.Background(), "任意", noopStep); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if eng.firstDecisionOutcomeThisTurn != firstDecisionFailUnknown {
		t.Errorf("outcome = %q, want %q", eng.firstDecisionOutcomeThisTurn, firstDecisionFailUnknown)
	}
}

// GATE (finding #1 + #2): continue-without-write must NOT pollute conversation
// history (no ContinueWithoutWrite tool_call / ack reaches e.messages, so the
// answer round reuses the identical pre-decision snapshot and hot == cold), and
// the probe must NOT be charged to the normal ReAct round count or the per-turn
// answer token budget.
func TestFirstDecisionContinueDoesNotPolluteHistoryOrBudget(t *testing.T) {
	withForcedFirstDecision(t)
	llmc := &capturingLLM{resps: []*llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("c1", continueWithoutWriteName, "{}")}, Usage: llm.TokenUsage{TotalTokens: 100}},
		{Content: "这是最终回答。", Usage: llm.TokenUsage{TotalTokens: 50}},
	}}
	eng := NewWithDeps(llmc, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 0)

	reply, err := eng.Chat(context.Background(), "怎么创建一台实例？", noopStep)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if reply != "这是最终回答。" {
		t.Fatalf("reply = %q, want the answer-round reply", reply)
	}
	// #1: no first-decision control message may have entered history — not the
	// ContinueWithoutWrite tool_call, not a tool response for it. A pure
	// continue→answer turn is exactly [user, assistant-final] with no tool traffic.
	for _, m := range eng.messages {
		if m.Role == openai.ChatMessageRoleTool {
			t.Errorf("continue turn leaked a tool message into history: %+v", m)
		}
		for _, tc := range m.ToolCalls {
			t.Errorf("continue turn leaked a tool_call (%s) into history", tc.Function.Name)
		}
	}
	// #2: the probe's 100 tokens are excluded from the answer budget (only the
	// answer round's 50 counts), and it did not consume a ReAct round.
	if eng.turnTokensConsumed != 50 {
		t.Errorf("turnTokensConsumed = %d, want 50 (the 100-token probe must be excluded)", eng.turnTokensConsumed)
	}
	if got := eng.ReactRoundsThisTurn(); got != 1 {
		t.Errorf("react_rounds = %d, want 1 (the probe is not a ReAct round)", got)
	}
}

// GATE (finding #3): when the provider silently drops the forcing and retries
// auto (ForcedToolChoiceDegraded), the turn must degrade to the normal
// full-window loop — NOT fail-closed a normal query — and record the fallback.
func TestFirstDecisionProviderFallbackDegradesToNormalLoop(t *testing.T) {
	withForcedFirstDecision(t)
	llmc := &capturingLLM{resps: []*llm.ChatResponse{
		{Content: "auto 兜底的自然语言", ForcedToolChoiceDegraded: true},
		{Content: "这是正常回答。"},
	}}
	eng := NewWithDeps(llmc, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 0)

	reply, err := eng.Chat(context.Background(), "帮我创建一台实例", noopStep)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if eng.firstDecisionOutcomeThisTurn != firstDecisionDegradedProviderFallback {
		t.Errorf("outcome = %q, want %q", eng.firstDecisionOutcomeThisTurn, firstDecisionDegradedProviderFallback)
	}
	if reply == firstDecisionFailReply {
		t.Fatalf("a provider-degraded turn must NOT be fail-closed refused")
	}
	if reply != "这是正常回答。" {
		t.Errorf("reply = %q, want the normal answer-loop reply", reply)
	}
	if len(llmc.reqs) < 2 {
		t.Fatalf("expected a normal answer round after the degrade; got %d requests", len(llmc.reqs))
	}
	// The degraded answer round runs the FULL window (as if the flag were off):
	// reads present, and — mutating on — Request* proposals still available.
	if !windowHasReadOrRAG(llmc.reqs[1].Tools) || !windowHasProposal(llmc.reqs[1].Tools) {
		t.Errorf("degraded answer window must be the full un-forced window; got %v", windowNames(llmc.reqs[1].Tools))
	}
}

// GATE: capability degradation is observable — when the model does not support
// required tool_choice, the forced first decision is skipped (marked degraded)
// and NOT counted as a structural guarantee.
func TestFirstDecisionDegradesWhenRequiredUnsupported(t *testing.T) {
	withForcedFirstDecision(t)
	llmc := &capturingLLM{resps: []*llm.ChatResponse{
		{Content: "普通回答。"},
	}}
	eng := NewWithDeps(llmc, &mockExecutor{}, nil)
	eng.supportsRequiredToolChoice = false // simulate an unsupported model/key
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 0)

	if _, err := eng.Chat(context.Background(), "帮我创建一台实例", noopStep); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if eng.firstDecisionOutcomeThisTurn != firstDecisionDegradedUnsupported {
		t.Errorf("outcome = %q, want %q", eng.firstDecisionOutcomeThisTurn, firstDecisionDegradedUnsupported)
	}
	// The round-0 window is the normal full window (reads present), not forced.
	if llmc.reqs[0].ToolChoice == "required" {
		t.Errorf("degraded path must not force tool_choice=required")
	}
}

// forcedCallCount counts the forced first-decision probes among the recorded
// requests. tool_choice="required" is unique to the forced first-decision window
// (monitor / SearchKnowledge forcing names a specific function, never "required"),
// so this reliably separates probe calls from the un-forced answer round.
func forcedCallCount(reqs []llm.ChatRequest) int {
	n := 0
	for _, r := range reqs {
		if r.ToolChoice == "required" {
			n++
		}
	}
	return n
}

// GATE (fail_no_tool bounded retry): a zero-tool response under `required` is a
// known provider instability, not a decision — it is retried EXACTLY ONCE with
// the identical restricted window, and a recovered proposal on the retry is the
// authoritative first decision (seeded straight to the resolver, no read/RAG
// preceded it on either probe). The zero-tool event stays observable via
// firstDecisionRetryFirstOutcome even though the final outcome is a request:.
func TestFirstDecisionZeroToolRetriesOnceThenSucceeds(t *testing.T) {
	withForcedFirstDecision(t)
	llmc := &capturingLLM{resps: []*llm.ChatResponse{
		{Content: ""}, // forced call 1: zero tools (empty) — a transient provider drop
		{ToolCalls: []openai.ToolCall{toolCall("r1", "RequestStopInstance", `{"UHostId":"uhost-x"}`)}}, // retry: the write proposal
		{Content: "已处理。"}, // answer round after the seeded proposal
	}}
	eng := NewWithDeps(llmc, &mockExecutor{}, func(string, map[string]any) bool { return false })
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 0)

	if _, err := eng.Chat(context.Background(), "帮我关掉 uhost-x", noopStep); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	// Exactly one bounded retry: two forced probes, not zero and not two retries.
	if got := forcedCallCount(llmc.reqs); got != 2 {
		t.Errorf("forced first-decision calls = %d, want 2 (one bounded retry)", got)
	}
	// The zero-tool first probe is still recorded even though the retry recovered.
	if eng.firstDecisionRetryFirstOutcome != firstDecisionFailNoToolEmpty {
		t.Errorf("retry-first outcome = %q, want %q", eng.firstDecisionRetryFirstOutcome, firstDecisionFailNoToolEmpty)
	}
	if got := eng.firstDecisionOutcomeThisTurn; got != "request:RequestStopInstance" {
		t.Errorf("final outcome = %q, want request:RequestStopInstance (retry recovered)", got)
	}
	if !eng.actionProposalRanThisTurn {
		t.Errorf("the recovered proposal must dispatch straight to the action-proposal resolver")
	}
	if !eng.writeWindowClosedThisTurn {
		t.Errorf("write window must be closed after the recovered write proposal")
	}
	// No read/RAG preceded the proposal on either the first probe or the retry.
	if windowHasReadOrRAG(llmc.reqs[0].Tools) || windowHasReadOrRAG(llmc.reqs[1].Tools) {
		t.Errorf("no read/RAG may appear in the forced window; got %v / %v",
			windowNames(llmc.reqs[0].Tools), windowNames(llmc.reqs[1].Tools))
	}
}

// GATE (fail_no_tool bounded retry): a SECOND consecutive zero-tool response
// fails CLOSED with the refusal — it does NOT degrade to the free full-window
// loop (no answer round runs), and the first zero-tool response never leaks into
// history (hot == cold). The final outcome distinguishes prose-refusal
// (fail_no_tool_text) from an empty provider drop (fail_no_tool_empty).
func TestFirstDecisionConsecutiveZeroToolFailsClosed(t *testing.T) {
	cases := []struct {
		name         string
		firstContent string
		retryContent string
		wantFirst    string
		wantOutcome  string
	}{
		{"both_empty", "", "", firstDecisionFailNoToolEmpty, firstDecisionFailNoToolEmpty},
		{"prose_refusal", "我先帮你查一下库存。", "还是先看看价格吧。", firstDecisionFailNoToolText, firstDecisionFailNoToolText},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withForcedFirstDecision(t)
			llmc := &capturingLLM{resps: []*llm.ChatResponse{
				{Content: tc.firstContent}, // forced call 1: zero tools
				{Content: tc.retryContent}, // retry: zero tools again
			}}
			eng := NewWithDeps(llmc, &mockExecutor{}, nil)
			eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 0)

			reply, err := eng.Chat(context.Background(), "帮我关掉 uhost-x", noopStep)
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			// Exactly one bounded retry, and NO answer round after fail-closed.
			if got := forcedCallCount(llmc.reqs); got != 2 {
				t.Errorf("forced first-decision calls = %d, want 2 (one bounded retry)", got)
			}
			if len(llmc.reqs) != 2 {
				t.Errorf("total LLM calls = %d, want 2 (fail-closed must NOT run a full-window answer round)", len(llmc.reqs))
			}
			if eng.firstDecisionRetryFirstOutcome != tc.wantFirst {
				t.Errorf("retry-first outcome = %q, want %q", eng.firstDecisionRetryFirstOutcome, tc.wantFirst)
			}
			if eng.firstDecisionOutcomeThisTurn != tc.wantOutcome {
				t.Errorf("final outcome = %q, want %q", eng.firstDecisionOutcomeThisTurn, tc.wantOutcome)
			}
			if reply != firstDecisionFailReply {
				t.Errorf("reply = %q, want the fail-closed refusal", reply)
			}
			// The first zero-tool prose must NOT have leaked into history — only the
			// single final refusal is persisted (hot == cold on rehydrate).
			var assistantContents []string
			for _, m := range eng.messages {
				if m.Role == openai.ChatMessageRoleTool {
					t.Errorf("fail-closed turn leaked a tool message into history: %+v", m)
				}
				if m.Role == openai.ChatMessageRoleAssistant {
					assistantContents = append(assistantContents, m.Content)
				}
			}
			if len(assistantContents) != 1 || assistantContents[0] != firstDecisionFailReply {
				t.Errorf("history assistant messages = %v, want exactly [refusal]", assistantContents)
			}
		})
	}
}

// GATE (spec: both probes count cost + rate-limit): the retry is a SECOND real
// forced call, so BOTH probes must be charged to the trace token observer AND the
// LLM rate-limit class — while NEITHER inflates the per-turn answer budget. Pins
// the doubled charge so a future retry that bypasses forcedFirstDecisionProbe
// (calling llmClient.Chat directly) cannot stay green.
func TestFirstDecisionRetryChargesBothProbes(t *testing.T) {
	withForcedFirstDecision(t)
	llmc := &capturingLLM{resps: []*llm.ChatResponse{
		{Content: "", Usage: llm.TokenUsage{TotalTokens: 100}},                                                                                  // probe 1: zero tools
		{ToolCalls: []openai.ToolCall{toolCall("r1", "RequestStopInstance", `{"UHostId":"uhost-x"}`)}, Usage: llm.TokenUsage{TotalTokens: 100}}, // retry: proposal
		{Content: "已处理。", Usage: llm.TokenUsage{TotalTokens: 50}},                                                                               // answer round
	}}
	eng := NewWithDeps(llmc, &mockExecutor{}, func(string, map[string]any) bool { return false })
	limiter := &scriptedRateLimiter{}
	eng.rateLimiter = limiter
	eng.rateLimitSubject = "sha256:subject"
	var observed []llm.TokenUsage
	eng.SetTokenUsageObserver(func(u llm.TokenUsage) { observed = append(observed, u) })
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 0)

	if _, err := eng.Chat(context.Background(), "帮我关掉 uhost-x", noopStep); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	// Cost: both 100-token probes reached the trace observer (2 observations),
	// proving each probe went through observeFirstDecisionTokens.
	probeObservations := 0
	for _, u := range observed {
		if u.TotalTokens == 100 {
			probeObservations++
		}
	}
	if probeObservations != 2 {
		t.Errorf("probe token observations = %d, want 2 (both forced probes must feed the trace observer); observed=%v", probeObservations, observed)
	}
	// ...but neither probe inflates the per-turn ANSWER budget — only the 50-token
	// answer round counts.
	if eng.turnTokensConsumed != 50 {
		t.Errorf("turnTokensConsumed = %d, want 50 (the two 100-token probes must be excluded from the answer budget)", eng.turnTokensConsumed)
	}
	// Rate limit: both probes debited the LLM class under the forced_first_decision
	// action (the main answer round debits main_react_chat, counted separately).
	forcedDebits := 0
	for _, r := range limiter.requests {
		if r.Class == governance.ClassLLM && r.Action == "forced_first_decision" {
			forcedDebits++
		}
	}
	if forcedDebits != 2 {
		t.Errorf("forced_first_decision rate-limit debits = %d, want 2 (both probes must charge the limiter)", forcedDebits)
	}
}

// GATE (spec: masked zero-tool observability is per-turn): firstDecisionRetryFirstOutcome
// must RESET each turn. Turn 1 has a zero-tool-then-recover event (the field is
// set); turn 2 has no zero-tool probe, so the field must read "" again — otherwise
// the stale value would inflate the next turn's zero-tool count in the acceptance
// measurement. Fails iff the reset at the top of Chat is dropped.
func TestFirstDecisionRetryFirstOutcomeResetsAcrossTurns(t *testing.T) {
	withForcedFirstDecision(t)
	llmc := &capturingLLM{resps: []*llm.ChatResponse{
		// Turn 1: zero-tool first probe, then recover to continue-without-write.
		{Content: ""},
		{ToolCalls: []openai.ToolCall{toolCall("c1", continueWithoutWriteName, "{}")}},
		{Content: "答案一。"},
		// Turn 2: a clean continue on the first probe — no zero-tool, no retry.
		{ToolCalls: []openai.ToolCall{toolCall("c2", continueWithoutWriteName, "{}")}},
		{Content: "答案二。"},
	}}
	eng := NewWithDeps(llmc, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 0)

	if _, err := eng.Chat(context.Background(), "怎么创建一台实例？", noopStep); err != nil {
		t.Fatalf("turn 1 Chat: %v", err)
	}
	if eng.firstDecisionRetryFirstOutcome != firstDecisionFailNoToolEmpty {
		t.Fatalf("turn 1 retry-first outcome = %q, want %q (a zero-tool event that recovered)", eng.firstDecisionRetryFirstOutcome, firstDecisionFailNoToolEmpty)
	}

	if _, err := eng.Chat(context.Background(), "4090 现在有库存吗", noopStep); err != nil {
		t.Fatalf("turn 2 Chat: %v", err)
	}
	if got := eng.FirstDecisionRetryFirstOutcome(); got != "" {
		t.Errorf("turn 2 retry-first outcome = %q, want \"\" (must reset each turn; stale value inflates the zero-tool count)", got)
	}
}
