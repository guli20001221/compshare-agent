package server

import (
	"context"
	"strings"
	"testing"

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
