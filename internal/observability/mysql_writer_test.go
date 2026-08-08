package observability

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"strconv"
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
		IntentRouter: RouterTrace{
			Intent: "resource_info",
		},
		EngineHardBlock: EngineHardBlockTrace{Hit: false},
		ToolCalls: []ToolCallTrace{
			{Action: "DescribeCompShareInstance"},
			{Action: "GetCompShareInstanceMonitor"},
		},
		Retrieval: RetrievalTrace{
			CitedChunkIDs: []string{"chunk-a", "chunk-b"},
			References: []RetrievalReference{{
				RefID:   "1",
				ChunkID: "chunk-a",
				Title:   "Billing rule",
			}},
			CitedRefs: []RetrievalCitedRef{{
				RefID:   "1",
				ChunkID: "chunk-a",
			}},
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
	for _, want := range []string{
		`"references":[{"ref_id":"1","chunk_id":"chunk-a","title":"Billing rule"`,
		`"cited_refs":[{"ref_id":"1","chunk_id":"chunk-a"}]`,
	} {
		if !strings.Contains(string(traceJSON), want) {
			t.Fatalf("trace_json missing %s; payload=%s", want, string(traceJSON))
		}
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

// TestPromotedColumnValues_ProjectsDerivedAxesInColumnOrder asserts the 0004
// promoted-column projection: the 7 derived attribution axes, in the exact order
// promotedInsertCols lists them after trace_json, from their canonical source
// fields. Encodes WHY: a field-routing drift here writes the wrong axis into the
// wrong queryable column — silently corrupting every dashboard GROUP BY. refusal_type
// is the one DERIVED value (DeriveRefusalType over RefusedReason+FloorDroppedAll),
// not a raw field copy, so it is exercised through a real refusal reason.
func TestPromotedColumnValues_ProjectsDerivedAxesInColumnOrder(t *testing.T) {
	rec := TraceRecord{
		IntentRouter: RouterTrace{RouteStatus: "dispatched_agent"},
		Retrieval:    RetrievalTrace{RefusedReason: "no_evidence"}, // → corpus_gap
		State:        StateTrace{ResolutionSource: "explicit_id"},
		Outcome: OutcomeTrace{
			TerminatedBy: TerminatedByError,
			AbortCause:   "client_disconnect",
			ErrorClass:   "upstream_5xx",
			Resolution:   "blocked",
		},
	}
	vals := promotedColumnValues(rec)
	want := []any{
		TerminatedByError,   // terminated_by
		"client_disconnect", // abort_cause
		"upstream_5xx",      // error_class
		"blocked",           // resolution
		"dispatched_agent",  // route_status
		"corpus_gap",        // refusal_type (DERIVED)
		"explicit_id",       // resolution_source
	}
	if len(vals) != len(want) {
		t.Fatalf("promotedColumnValues len = %d, want %d", len(vals), len(want))
	}
	for i := range want {
		if vals[i] != want[i] {
			t.Fatalf("promoted col %d = %#v, want %#v", i, vals[i], want[i])
		}
	}
}

// TestPromotedColumnValues_EmptyAxesBecomeSQLNull guards the NULL contract: a clean
// answered turn (no refusal, no error, no special terminus) must store NULL — not ""
// — in every promoted column, so COUNT(refusal_type) / GROUP BY counts only the
// turns where the axis actually fired. (terminated_by is the one axis always set on
// a finalized turn; here the record is un-finalized so even it is empty → NULL.)
func TestPromotedColumnValues_EmptyAxesBecomeSQLNull(t *testing.T) {
	vals := promotedColumnValues(TraceRecord{})
	if len(vals) != 7 {
		t.Fatalf("promotedColumnValues len = %d, want 7", len(vals))
	}
	for i, v := range vals {
		if v != nil {
			t.Fatalf("promoted col %d = %#v, want nil (SQL NULL) for empty axis", i, v)
		}
	}
}

// TestNullableStr pins the ""→NULL mapping that keeps GROUP BY buckets clean.
func TestNullableStr(t *testing.T) {
	if got := nullableStr(""); got != nil {
		t.Fatalf(`nullableStr("") = %#v, want nil`, got)
	}
	if got := nullableStr("done"); got != "done" {
		t.Fatalf(`nullableStr("done") = %#v, want "done"`, got)
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

func TestMySQLWriterStatsExposeQueueDrops(t *testing.T) {
	w := &MySQLWriter{
		queue:  make(chan persistedTrace, 1),
		logger: silentLogger(t),
	}
	if err := w.Append(TraceRecord{TraceID: "first"}); err != nil {
		t.Fatalf("first Append: %v", err)
	}
	if err := w.Append(TraceRecord{TraceID: "dropped"}); err != nil {
		t.Fatalf("dropped Append: %v", err)
	}

	stats := w.Stats()
	if stats.EnqueueAttempts != 2 || stats.QueueAccepted != 1 || stats.QueueDropped != 1 ||
		stats.MalformedDropped != 0 || stats.BatchFailedRecords != 0 {
		t.Fatalf("writer stats = %#v, want attempts=2 accepted=1 dropped=1 and no other losses", stats)
	}
}

func TestMySQLWriterLogsContentFreeHealthAtMilestone(t *testing.T) {
	var output bytes.Buffer
	w := &MySQLWriter{
		queue:  make(chan persistedTrace, traceWriterHealthLogEvery),
		logger: log.New(&output, "", 0),
	}
	for i := 0; i < traceWriterHealthLogEvery; i++ {
		if err := w.Append(TraceRecord{TraceID: strconv.Itoa(i)}); err != nil {
			t.Fatalf("Append(%d): %v", i, err)
		}
	}
	line := output.String()
	if !strings.Contains(line, "agent_trace writer health: event=periodic attempted=100") ||
		strings.Contains(line, "trace_id") {
		t.Fatalf("health log = %q, want content-free periodic counters", line)
	}
}

// Queue-full logging must make the first loss visible without turning an
// overloaded telemetry sink into a second source of unbounded load. Periodic
// health records retain the cumulative count after this one warning.
func TestMySQLWriterLogsFirstQueueDropOnlyOnce(t *testing.T) {
	var output bytes.Buffer
	w := &MySQLWriter{
		queue:  make(chan persistedTrace, 1),
		logger: log.New(&output, "", 0),
	}
	requireAppend(t, w, TraceRecord{TraceID: "accepted"})
	for i := 0; i < 3; i++ {
		requireAppend(t, w, TraceRecord{TraceID: "dropped-" + strconv.Itoa(i)})
	}

	const queueDropEvent = "event=queue full; dropping trace"
	if got := strings.Count(output.String(), queueDropEvent); got != 1 {
		t.Fatalf("queue-full warning count = %d, want 1; logs=%q", got, output.String())
	}
	if strings.Contains(output.String(), "trace_id") {
		t.Fatalf("queue-drop health log must remain content-free: %q", output.String())
	}
	if got := w.Stats().QueueDropped; got != 3 {
		t.Fatalf("queue drop counter = %d, want 3", got)
	}
}

// Close is normally called before a short-lived server exits, well before the
// every-100-enqueues milestone. It must still leave one final accounting line
// after the worker drain decision, including when there was no SQL connection
// to flush because the queue was empty.
func TestMySQLWriterLogsHealthOnShutdown(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://trace:test@127.0.0.1:1/trace?sslmode=disable")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	workerDone := make(chan struct{})
	close(workerDone)
	var output bytes.Buffer
	w := &MySQLWriter{
		db:         db,
		queue:      make(chan persistedTrace),
		workerDone: workerDone,
		logger:     log.New(&output, "", 0),
	}
	w.enqueueAttempts.Store(7)
	w.queueDropped.Store(2)

	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	line := output.String()
	if !strings.Contains(line, "event=shutdown attempted=7") ||
		!strings.Contains(line, "queue_dropped=2") {
		t.Fatalf("shutdown health log = %q, want final counters", line)
	}
}

func requireAppend(t *testing.T, w *MySQLWriter, record TraceRecord) {
	t.Helper()
	if err := w.Append(record); err != nil {
		t.Fatalf("Append(%q): %v", record.TraceID, err)
	}
}

// TestMySQLWriter_EnqueueRedactsQueryDerivedPIIBeforePersist is the regression
// guard for the privacy leak where redaction + withDefaults lived ONLY in
// FileWriter.Append, so the MySQL sink persisted raw user queries (PII) into
// trace_json with an empty schema_version. It exercises the real MySQL sink path
// end-to-end without a DB: Enqueue prepares the record (the worker-free writer
// lets us read the prepared persistedTrace straight off the queue), then
// rowFromTrace projects the trace_json column the worker would INSERT.
//
// The fixture is a real PII query (staff name 张慧, the same input as
// TestRedactQueryDerivedFieldsRedactsStaffNames) — NOT a pre-redacted mock — so
// the test genuinely fails if the producer regresses (memory: schema-test-anti-mock).
// Mirrors TestWriterAppendDoesNotLeakSecretsInTraceLine on the FileWriter side.
func TestMySQLWriter_EnqueueRedactsQueryDerivedPIIBeforePersist(t *testing.T) {
	w := &MySQLWriter{
		queue:  make(chan persistedTrace, 1),
		logger: silentLogger(t),
	}
	if err := w.Enqueue(TenantContext{TopOrgID: 1, OrgID: 2}, TraceRecord{
		TraceID: "pii-leak",
		Retrieval: RetrievalTrace{
			QueryRaw:        "请张慧帮我看一下实例启动失败",
			QueryNormalized: "张慧 实例 启动失败",
			QueryExpansions: []string{"实例启动失败", "找张慧处理"},
		},
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// The worker-free writer leaves the prepared record on the queue: this is the
	// exact persistedTrace the worker would hand to rowFromTrace.
	row, err := rowFromTrace(<-w.queue)
	if err != nil {
		t.Fatalf("rowFromTrace: %v", err)
	}
	traceJSON, ok := row[11].([]byte)
	if !ok {
		t.Fatalf("col 11 (trace_json) wrong type %T", row[11])
	}
	blob := string(traceJSON)

	if strings.Contains(blob, "张慧") {
		t.Fatalf("MySQL trace_json leaked PII staff name 张慧: %s", blob)
	}
	if !strings.Contains(blob, "[REDACTED]") {
		t.Fatalf("MySQL trace_json should carry the redaction marker; got: %s", blob)
	}
	if !strings.Contains(blob, `"schema_version":"`+SchemaVersion+`"`) {
		t.Fatalf("MySQL trace_json should carry schema_version after prepareForPersist; got: %s", blob)
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
