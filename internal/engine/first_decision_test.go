package engine

import (
	"context"
	"strings"
	"testing"

	openai "github.com/sashabaranov/go-openai"

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
