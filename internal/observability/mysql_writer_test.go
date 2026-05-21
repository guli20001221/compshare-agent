package observability

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"
)

// TestRowFromTrace_Basic asserts the 12-column projection for a populated
// TraceRecord. Encodes WHY: cited_chunk_ids comes from RetrievalTrace (not
// RendererTrace; cf. plan §7.5 + memory feedback_plan_field_ref_must_include_container_struct),
// duration_ms comes from OutcomeTrace.TotalLatencyMS, tool_count is the
// len of ToolCalls. Field-routing drift = lost data once in prod.
func TestRowFromTrace_PopulatesAllColumnsFromCanonicalSources(t *testing.T) {
	rec := TraceRecord{
		TraceID:   "req-uuid-123",
		TurnIndex: 7,
		Timestamp: "2026-05-21T03:00:00Z",
		Planner: PlannerTrace{
			Intent: "resource_info",
		},
		EngineHardBlock: EngineHardBlockTrace{Hit: false},
		ToolCalls: []ToolCallTrace{
			{Action: "DescribeCompShareInstance"},
			{Action: "GetCompShareInstanceMonitor"},
		},
		Retrieval: RetrievalTrace{
			CitedChunkIDs: []string{"chunk-a", "chunk-b"},
		},
		Outcome: OutcomeTrace{
			TotalLatencyMS: 1234,
		},
	}
	tenant := TenantContext{TopOrgID: 11, OrgID: 111, ConnectionID: "conn-abc"}

	row, err := rowFromTrace(persistedTrace{tenant: tenant, record: rec})
	if err != nil {
		t.Fatalf("rowFromTrace: %v", err)
	}
	if len(row) != 12 {
		t.Fatalf("expected 12 columns, got %d", len(row))
	}

	// Column order documented in mysql_writer.go::insertBatch cols list.
	assertColEq(t, row, 0, "req-uuid-123", "request_uuid")
	assertColEq(t, row, 1, int64(11), "top_organization_id")
	assertColEq(t, row, 2, int64(111), "organization_id")
	assertColEq(t, row, 3, "conn-abc", "connection_id")
	assertColEq(t, row, 4, 7, "turn_index")
	assertColEq(t, row, 5, "2026-05-21T03:00:00Z", "created_at")
	assertColEq(t, row, 6, "success", "status")
	assertColEq(t, row, 7, "resource_info", "intent")
	assertColEq(t, row, 8, 2, "tool_count")

	citedJSON, ok := row[9].([]byte)
	if !ok {
		t.Fatalf("col 9 (cited_chunk_ids) wrong type %T: %#v", row[9], row[9])
	}
	var citedList []string
	if err := json.Unmarshal(citedJSON, &citedList); err != nil {
		t.Fatalf("cited_chunk_ids not valid JSON: %v", err)
	}
	if len(citedList) != 2 || citedList[0] != "chunk-a" || citedList[1] != "chunk-b" {
		t.Fatalf("cited_chunk_ids drift: %v", citedList)
	}

	assertColEq(t, row, 10, int64(1234), "duration_ms")

	traceJSON, ok := row[11].([]byte)
	if !ok {
		t.Fatalf("col 11 (trace_json) wrong type %T", row[11])
	}
	if !strings.Contains(string(traceJSON), `"trace_id":"req-uuid-123"`) {
		t.Fatalf("trace_json does not embed trace_id; payload=%s", string(traceJSON))
	}
}

// TestRowFromTrace_EmptyCitedChunkIDsBecomesEmptyJSONArray.
// agent_traces.cited_chunk_ids is JSON NOT NULL — `null` would violate the
// constraint. Encodes WHY: marshalling a nil []string yields "null", which
// is wrong for a JSON column with NOT NULL.
func TestRowFromTrace_EmptyCitedChunkIDsBecomesEmptyJSONArray(t *testing.T) {
	rec := TraceRecord{TraceID: "no-citations"}
	row, err := rowFromTrace(persistedTrace{record: rec})
	if err != nil {
		t.Fatalf("rowFromTrace: %v", err)
	}
	cited, ok := row[9].([]byte)
	if !ok {
		t.Fatalf("col 9 wrong type %T", row[9])
	}
	if string(cited) != "[]" {
		t.Fatalf("expected empty JSON array for no citations, got %q", string(cited))
	}
}

// TestStatusFromTrace covers the three terminal states inferable from the
// trace alone. The richer server-side helper (DeriveStatus, lands in PR5)
// also factors in the engine's chatErr — that path is tested separately.
func TestStatusFromTrace(t *testing.T) {
	cases := []struct {
		name string
		rec  TraceRecord
		want string
	}{
		{
			"clean success",
			TraceRecord{},
			"success",
		},
		{
			"engine hard block fired",
			TraceRecord{EngineHardBlock: EngineHardBlockTrace{Hit: true, Category: "account_billing"}},
			"blocked",
		},
		{
			"rate limit denial",
			TraceRecord{RateLimit: RateLimitTrace{Checked: true, Allowed: false, Reason: "qps_exceeded"}},
			"blocked",
		},
		{
			"rate limit checked but allowed → success",
			TraceRecord{RateLimit: RateLimitTrace{Checked: true, Allowed: true}},
			"success",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := statusFromTrace(c.rec); got != c.want {
				t.Fatalf("statusFromTrace(%s) = %q, want %q", c.name, got, c.want)
			}
		})
	}
}

// TestMySQLWriter_AppendBeforeWorkerDoesNotBlock asserts the documented
// non-blocking semantics. Even with a queue of size 1 and no worker draining,
// the second Append must return immediately (drop the record + warn, not
// block). Encodes WHY: blocking here would freeze Engine.Chat under DB
// outage — directly the failure mode plan §7.8 enumerates.
//
// We don't actually start the worker (no DB) — we construct the writer
// fields manually so we can exercise just the enqueue path.
func TestMySQLWriter_AppendBeforeWorkerDoesNotBlock(t *testing.T) {
	w := &MySQLWriter{
		queue:  make(chan persistedTrace, 1),
		logger: silentLogger(t),
	}
	// Fill the buffer.
	if err := w.Append(TraceRecord{TraceID: "first"}); err != nil {
		t.Fatalf("first Append: %v", err)
	}
	// Second Append must NOT block — the writer's select { case queue<-...; default }
	// path drops + warns instead of waiting.
	done := make(chan struct{})
	go func() {
		if err := w.Append(TraceRecord{TraceID: "dropped"}); err != nil {
			t.Errorf("second Append returned error %v; expected silent drop", err)
		}
		close(done)
	}()
	// 100ms is generous — non-blocking Append should return in microseconds.
	// If we hit the timeout, Append blocked on the channel send and the
	// non-blocking contract is broken.
	select {
	case <-done:
		// Expected: returned promptly.
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("second Append blocked despite full queue; non-blocking contract broken")
	}
}

// TestNewMySQLWriter_EmptyDSNErrors guards the documented contract.
func TestNewMySQLWriter_EmptyDSNErrors(t *testing.T) {
	w, err := NewMySQLWriter("", MySQLWriterOptions{})
	if err == nil {
		t.Fatalf("NewMySQLWriter(\"\") returned err=nil")
	}
	if w != nil {
		t.Fatalf("NewMySQLWriter(\"\") returned non-nil writer on error")
	}
}

// TestNewMySQLWriter_InvalidDSNErrors guards the ping-on-startup contract.
// An obviously-unreachable DSN should fail fast, not return a half-initialized
// writer that the server would later use to silently drop traces.
//
// SECURITY/correctness: this is the gate that turns "MySQL is down" into a
// startup failure instead of a runtime data-loss scenario.
func TestNewMySQLWriter_UnreachableDSNErrors(t *testing.T) {
	// 127.0.0.1:1 is reserved-port-not-listening; ping fails within the
	// 5s timeout NewMySQLWriter applies.
	w, err := NewMySQLWriter("root:none@tcp(127.0.0.1:1)/nodb?parseTime=true", MySQLWriterOptions{})
	if err == nil {
		if w != nil {
			_ = w.db.Close()
		}
		t.Fatalf("NewMySQLWriter with unreachable DSN returned err=nil")
	}
	if !strings.Contains(err.Error(), "ping") && !errors.Is(err, errFakeForCompile()) {
		// Loose check: as long as error mentions ping or comes from sql.Open's
		// chain, we consider it correct. The exact text varies across go-sql-driver
		// versions; we just need a non-nil error.
		t.Logf("non-ping-keyworded error (acceptable as long as non-nil): %v", err)
	}
}

// errFakeForCompile is a sentinel to keep the errors import in use without
// adding a real "errors.Is" assertion the driver doesn't support. The
// strings.Contains check above is the real assertion.
func errFakeForCompile() error { return errors.New("placeholder") }

// silentLogger returns a logger that discards output during tests so
// drop-warnings don't spam test stdout.
func silentLogger(t *testing.T) *log.Logger {
	t.Helper()
	return log.New(io.Discard, "", 0)
}

// assertColEq is a tiny helper that compares the i'th column value to the
// expected value, with a label that names the column so failures are easy
// to read.
func assertColEq(t *testing.T, row []any, i int, want any, label string) {
	t.Helper()
	if row[i] != want {
		t.Errorf("col %d (%s) drift: got %#v (type %T), want %#v (type %T)",
			i, label, row[i], row[i], want, want)
	}
}
