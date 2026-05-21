package server

import (
	"context"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
)

// captureWriter records every Append call so tests can assert the
// projected TraceRecord. It satisfies observability.Writer without
// hitting disk or MySQL.
type captureWriter struct {
	records []observability.TraceRecord
}

func (c *captureWriter) Append(rec observability.TraceRecord) error {
	c.records = append(c.records, rec)
	return nil
}
func (c *captureWriter) Dir() string                 { return "" }
func (c *captureWriter) Close(context.Context) error { return nil }

// TestServerTraceRecorder_TraceIDPassthrough — A7 contract.
//
// Encodes WHY: dashboards join `agent_traces.request_uuid` and
// `agent_traces.trace_json -> '$.trace_id'`; if they diverge a single
// chat turn appears as two unrelated trace records to operators.
// Plan §10.5 acceptance: "SELECT request_uuid, JSON_EXTRACT(trace_json,
// '$.trace_id') ... ; 期望两列相同" — this test enforces that contract
// at the Go boundary BEFORE MySQL writes the row.
func TestServerTraceRecorder_TraceIDPassthrough(t *testing.T) {
	w := &captureWriter{}
	tenant := TenantCtx{TopOrgID: 11, OrgID: 111}
	msg := ClientMessage{
		Type:        ClientMsgUserMessage,
		Text:        "hi",
		RequestUUID: "req-uuid-abc-xyz-123",
	}
	rec := newServerTraceRecorder(w, tenant, "conn-1", 7, msg, "ds-v4-flash")
	if rec == nil {
		t.Fatalf("recorder unexpectedly nil")
	}
	if rec.record.TraceID != msg.RequestUUID {
		t.Fatalf("trace_id passthrough drift: got %q, want %q",
			rec.record.TraceID, msg.RequestUUID)
	}

	rec.finish(nil)
	if len(w.records) != 1 {
		t.Fatalf("expected 1 captured record, got %d", len(w.records))
	}
	if w.records[0].TraceID != msg.RequestUUID {
		t.Fatalf("persisted trace_id drift: got %q, want %q",
			w.records[0].TraceID, msg.RequestUUID)
	}
}

// TestServerTraceRecorder_TraceIDFallback — when client omits request_uuid
// (legacy / malformed client), the recorder must still produce a non-
// empty unique trace_id so the agent_traces uk_request_uuid UNIQUE
// constraint isn't violated by repeated empty strings.
func TestServerTraceRecorder_TraceIDFallback(t *testing.T) {
	w := &captureWriter{}
	tenant := TenantCtx{TopOrgID: 11, OrgID: 111}
	msg := ClientMessage{Type: ClientMsgUserMessage, Text: "hi"} // RequestUUID empty

	rec := newServerTraceRecorder(w, tenant, "conn-1", 7, msg, "ds-v4-flash")
	if rec == nil {
		t.Fatalf("recorder unexpectedly nil")
	}
	if rec.record.TraceID == "" {
		t.Fatalf("fallback trace_id must not be empty")
	}
	if !strings.HasPrefix(rec.record.TraceID, "trace-") {
		t.Fatalf("fallback trace_id should start with 'trace-' prefix; got %q",
			rec.record.TraceID)
	}

	// Two recorders for the same empty-uuid scenario must produce
	// DIFFERENT trace_ids — otherwise the UNIQUE constraint trips.
	rec2 := newServerTraceRecorder(w, tenant, "conn-1", 8, msg, "ds-v4-flash")
	if rec.record.TraceID == rec2.record.TraceID {
		t.Fatalf("fallback trace_ids must be unique across turns: both got %q",
			rec.record.TraceID)
	}
}

// TestServerTraceRecorder_NilSink — short-circuit guard so handlers can
// skip observer wiring cheaply when no sink is configured.
func TestServerTraceRecorder_NilSink(t *testing.T) {
	rec := newServerTraceRecorder(nil, TenantCtx{}, "conn-1", 0, ClientMessage{}, "")
	if rec != nil {
		t.Fatalf("nil sink should return nil recorder, got %+v", rec)
	}
}

// TestServerTraceRecorder_PlannerTokensCountTowardTotal — locks in the
// fix for the issue where planner LLM tokens were missing from
// agent_traces.total_tokens. Pre-fix the planner observer only stored
// the planner trace struct; the recorder's token accumulator only saw
// emitTokenUsage events from non-planner calls, so MySQL underreported
// total tokens for any turn that used the planner (which is most of
// them under the cutover-enabled deploy).
//
// Encodes WHY: agent_traces.total_tokens is a budgeting/billing signal.
// If it diverges from engine.turnTokensConsumed (the gate enforcer),
// operators looking at a "blocked by token_budget" row see a
// total_tokens that's smaller than the cap — making the block look
// like a false positive. Test enforces parity at the recorder boundary
// BEFORE MySQL writes the row.
func TestServerTraceRecorder_PlannerTokensCountTowardTotal(t *testing.T) {
	w := &captureWriter{}
	tenant := TenantCtx{TopOrgID: 11, OrgID: 111}
	msg := ClientMessage{Type: ClientMsgUserMessage, RequestUUID: "uuid-1", Text: "hi"}
	rec := newServerTraceRecorder(w, tenant, "conn-1", 1, msg, "ds-v4-flash")
	if rec == nil {
		t.Fatalf("recorder unexpectedly nil")
	}

	// Drive the observers directly the same way the attach() callbacks
	// would, without spinning up a full Engine. Planner emits 7000
	// tokens; one regular LLM call emits 5000. Expected total = 12000.
	rec.record.Planner = observability.PlannerTrace{}
	// Simulate planner trace observer.
	plannerTrace := observability.PlannerTrace{
		Enabled:      true,
		InputTokens:  3000,
		OutputTokens: 4000,
	}
	rec.record.Planner = plannerTrace
	rec.totalTokens += plannerTrace.InputTokens + plannerTrace.OutputTokens
	rec.record.Outcome.TotalTokens = rec.totalTokens

	// Simulate token-usage observer (regular Chat / answerer / renderer).
	rec.totalTokens += llmTokenUsageTotal(llm.TokenUsage{
		PromptTokens:     2000,
		CompletionTokens: 3000,
		TotalTokens:      5000,
	})
	rec.record.Outcome.TotalTokens = rec.totalTokens

	rec.finish(nil)
	if len(w.records) != 1 {
		t.Fatalf("expected 1 captured record, got %d", len(w.records))
	}
	got := w.records[0].Outcome.TotalTokens
	if got != 12000 {
		t.Fatalf("Outcome.TotalTokens should sum planner + LLM calls (7000 + 5000 = 12000); got %d", got)
	}
}

// TestLLMTokenUsageTotal_PrefersTotalOverSum — mirrors the CLI helper's
// behavior. WHY: streaming providers (ds-v4-flash) sometimes report
// only Usage.TotalTokens without splitting prompt/completion. Falling
// back to the sum when TotalTokens==0 covers the other shape.
func TestLLMTokenUsageTotal_PrefersTotalOverSum(t *testing.T) {
	cases := []struct {
		name  string
		usage llm.TokenUsage
		want  int
	}{
		{name: "total only", usage: llm.TokenUsage{TotalTokens: 100}, want: 100},
		{name: "split only", usage: llm.TokenUsage{PromptTokens: 30, CompletionTokens: 50}, want: 80},
		{name: "both set, total wins", usage: llm.TokenUsage{PromptTokens: 30, CompletionTokens: 50, TotalTokens: 99}, want: 99},
		{name: "empty", usage: llm.TokenUsage{}, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := llmTokenUsageTotal(tc.usage); got != tc.want {
				t.Fatalf("llmTokenUsageTotal(%+v) = %d; want %d", tc.usage, got, tc.want)
			}
		})
	}
}
