package observability

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	// PostgreSQL driver registered via blank import. The Server bootstrap (A3) is
	// responsible for verifying connectivity at startup so callers of this
	// package never see driver-registration failures. (Type/name retained as
	// MySQLWriter for call-site stability; backend is PostgreSQL.)
	_ "github.com/lib/pq"
)

// MySQLWriter is a Writer that persists TraceRecords into agent_traces in a
// MySQL 8.0 database. Designed for the console-deployment server path where
// trace volume can be ~10/s/pod and trace inserts MUST NOT block the engine
// reply loop.
//
// Buffering & back-pressure:
//   - Append is non-blocking: it pushes to a buffered queue and returns nil.
//   - When the queue is full, the record is DROPPED and a warning is logged.
//     This is the documented behavior per plan §7.8 (MySQL must never block
//     Engine.Chat). Production should alert on the warning rate.
//   - A worker goroutine drains the queue and inserts in batches sized by
//     batchSize OR flushed by flushPeriod, whichever comes first.
//
// Close drains the queue and shuts down the worker. Callers should invoke it
// at process shutdown; otherwise the buffered records are lost.
type MySQLWriter struct {
	db            *sql.DB
	queue         chan persistedTrace
	workerDone    chan struct{}
	batchSize     int
	flushPeriod   time.Duration
	retentionDays int
	logger        *log.Logger
	// promotedColumns is true when agent_traces has the 0004 outcome columns
	// (terminated_by, refusal_type, …). Probed once at startup. When false the
	// writer falls back to the legacy 12-column INSERT so a new binary on a DB
	// that has 0002 but not 0004 still ingests trace_json instead of failing
	// every batch on an unknown-column error (the deploy-order must-fix).
	promotedColumns bool

	// Writer health is intentionally metadata-only. Trace delivery is best
	// effort so it never delays an Agent reply; these counters make a degraded
	// sink visible instead of silently turning an optimization dataset into a
	// biased sample.
	enqueueAttempts    atomic.Uint64
	queueAccepted      atomic.Uint64
	queueDropped       atomic.Uint64
	malformedDropped   atomic.Uint64
	batchFailedRecords atomic.Uint64
	insertSucceeded    atomic.Uint64
}

// TraceWriterStats is a content-free snapshot of the asynchronous SQL sink.
// QueueAccepted means accepted into this process's queue, not committed to the
// database; InsertSucceeded counts records in batches whose INSERT completed
// without an error (an ON CONFLICT duplicate may still make no new row).
type TraceWriterStats struct {
	EnqueueAttempts    uint64
	QueueAccepted      uint64
	QueueDropped       uint64
	MalformedDropped   uint64
	BatchFailedRecords uint64
	InsertSucceeded    uint64
}

const traceWriterHealthLogEvery = 100

// persistedTrace bundles tenant context with the trace record. TraceRecord
// itself has no tenant field — callers add tenant identifiers via Enqueue
// (recommended; explicit context) or via Append (legacy/CLI; tenants are 0
// which is fine for file-style sinks but produces zeroed columns in MySQL).
type persistedTrace struct {
	tenant TenantContext
	record TraceRecord
}

// TenantContext is the per-request identity attached to a trace row when
// persisting to MySQL. Populated by the server WS handler before calling
// MySQLWriter.Enqueue. CLI path (Append) leaves these at zero.
type TenantContext struct {
	TopOrgID     int64
	OrgID        int64
	ConnectionID string
}

// MySQLWriterOptions tunes the buffering knobs. Sensible defaults are used
// when fields are zero.
type MySQLWriterOptions struct {
	QueueSize     int           // default 1024
	BatchSize     int           // default 50
	FlushPeriod   time.Duration // default 1s
	RetentionDays int           // default DefaultTraceRetentionDays (30); <=0 → default
	Logger        *log.Logger   // default log.Default()
}

// NewMySQLWriter opens a connection to the given DSN, pings to verify
// connectivity, and starts the background worker goroutine. Caller MUST
// Close to drain.
//
// The DSN MUST include charset=utf8mb4 to support emoji + Chinese in
// user_message / trace_json. parseTime=true is recommended so created_at
// round-trips as time.Time without conversion shims.
func NewMySQLWriter(dsn string, opts MySQLWriterOptions) (*MySQLWriter, error) {
	if dsn == "" {
		return nil, errors.New("mysql writer: dsn is empty")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("trace writer open: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("mysql writer ping: %w", err)
	}

	w := &MySQLWriter{
		db:            db,
		queue:         make(chan persistedTrace, defaultIfZero(opts.QueueSize, 1024)),
		workerDone:    make(chan struct{}),
		batchSize:     defaultIfZero(opts.BatchSize, 50),
		flushPeriod:   defaultDurationIfZero(opts.FlushPeriod, time.Second),
		retentionDays: defaultIfZero(opts.RetentionDays, DefaultTraceRetentionDays),
		logger:        defaultLogger(opts.Logger),
	}
	w.promotedColumns = detectPromotedColumns(db, w.logger)
	go w.run()
	return w, nil
}

// detectPromotedColumns probes once at startup whether agent_traces has the 0004
// outcome columns. When false, insertBatch uses the legacy 12-column INSERT so a
// new binary on a DB that has 0002 but not 0004 still ingests trace_json instead of
// failing every batch on an unknown-column error (the deploy-order must-fix). Any
// probe error is treated as "absent" — degrade safely, never block ingestion.
func detectPromotedColumns(db *sql.DB, logger *log.Logger) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.columns
		   WHERE table_schema = current_schema()
		     AND table_name = 'agent_traces'
		     AND column_name = 'terminated_by'`).Scan(&n)
	if err != nil {
		logger.Printf("mysql_writer: promoted-column probe failed (%v); using legacy 12-column INSERT", err)
		return false
	}
	if n == 0 {
		logger.Printf("mysql_writer: agent_traces missing promoted columns (run migration 0004); writing trace_json only")
		return false
	}
	logger.Printf("mysql_writer: agent_traces has promoted outcome columns; GROUP-BY columns enabled")
	return true
}

// Append satisfies the Writer interface. CLI / legacy callers use this when
// they do not need to attach tenant context. Equivalent to Enqueue with a
// zero TenantContext.
func (w *MySQLWriter) Append(record TraceRecord) error {
	return w.Enqueue(TenantContext{}, record)
}

// Enqueue is the preferred entry point for server paths: it carries tenant
// identifiers that the MySQL row schema requires (top_organization_id,
// organization_id, connection_id). Non-blocking; drops + warns if the queue
// is full.
func (w *MySQLWriter) Enqueue(tenant TenantContext, record TraceRecord) error {
	// Single choke point shared with FileWriter.Append: fill defaults + redact
	// query-derived PII BEFORE the record enters the queue, so neither an
	// in-memory queue dump nor the worker (rowFromTrace → trace_json) ever sees
	// raw user queries. Pre-fix this was absent → the MySQL sink persisted real
	// PII (staff names) unredacted with an empty schema_version.
	record = prepareForPersist(record, time.Now())
	attempt := w.enqueueAttempts.Add(1)
	select {
	case w.queue <- persistedTrace{tenant: tenant, record: record}:
		w.queueAccepted.Add(1)
		w.maybeLogHealth(attempt)
		return nil
	default:
		w.queueDropped.Add(1)
		w.logHealth("queue full; dropping trace")
		w.maybeLogHealth(attempt)
		return nil
	}
}

// Stats exposes delivery health to diagnostics and tests without exposing a
// TraceRecord, prompt, reply, identifier or tenant. It is safe to call while
// the worker is flushing.
func (w *MySQLWriter) Stats() TraceWriterStats {
	if w == nil {
		return TraceWriterStats{}
	}
	return TraceWriterStats{
		EnqueueAttempts:    w.enqueueAttempts.Load(),
		QueueAccepted:      w.queueAccepted.Load(),
		QueueDropped:       w.queueDropped.Load(),
		MalformedDropped:   w.malformedDropped.Load(),
		BatchFailedRecords: w.batchFailedRecords.Load(),
		InsertSucceeded:    w.insertSucceeded.Load(),
	}
}

func (w *MySQLWriter) maybeLogHealth(attempt uint64) {
	if attempt == 0 || attempt%traceWriterHealthLogEvery != 0 {
		return
	}
	w.logHealth("periodic")
}

func (w *MySQLWriter) logHealth(event string) {
	if w == nil || w.logger == nil {
		return
	}
	stats := w.Stats()
	w.logger.Printf("agent_trace writer health: event=%s attempted=%d queue_accepted=%d queue_dropped=%d malformed_dropped=%d batch_failed_records=%d insert_succeeded=%d",
		event,
		stats.EnqueueAttempts,
		stats.QueueAccepted,
		stats.QueueDropped,
		stats.MalformedDropped,
		stats.BatchFailedRecords,
		stats.InsertSucceeded,
	)
}

// Dir satisfies the Writer interface. MySQLWriter has no on-disk dir so the
// trace-dir cleanup logic in cmd/trace.go can skip it cleanly.
func (w *MySQLWriter) Dir() string { return "" }

// Close drains the queue and shuts down the worker goroutine, then closes
// the underlying database handle. The caller's context bounds the drain
// time; on timeout, in-flight records are abandoned.
func (w *MySQLWriter) Close(ctx context.Context) error {
	close(w.queue)
	select {
	case <-w.workerDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	return w.db.Close()
}

func (w *MySQLWriter) run() {
	defer close(w.workerDone)
	batch := make([]persistedTrace, 0, w.batchSize)
	tick := time.NewTicker(w.flushPeriod)
	defer tick.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		candidateCount, err := w.insertBatch(batch)
		if err != nil {
			w.batchFailedRecords.Add(uint64(candidateCount))
			w.logger.Printf("mysql_writer: batch insert failed (%d records): %v",
				candidateCount, err)
			w.logHealth("batch insert failed")
		} else {
			w.insertSucceeded.Add(uint64(candidateCount))
		}
		batch = batch[:0]
	}

	// Retention sweep: agent_traces has no TTL of its own (the JSONL sink expires
	// files via observability.Cleanup; the MySQL sink previously had no equivalent
	// and grew unbounded). Sweep once at startup (mirrors the file sink's
	// per-process cleanup) and then daily for the long-running server.
	w.sweepExpired()
	retentionTick := time.NewTicker(24 * time.Hour)
	defer retentionTick.Stop()

	for {
		select {
		case rec, ok := <-w.queue:
			if !ok {
				flush()
				return
			}
			batch = append(batch, rec)
			if len(batch) >= w.batchSize {
				flush()
			}
		case <-tick.C:
			flush()
		case <-retentionTick.C:
			w.sweepExpired()
		}
	}
}

// retentionCutoff is the created_at boundary below which rows are expired: rows
// strictly older than now - retentionDays. Pure so the boundary is unit-testable
// without a live DB.
func retentionCutoff(now time.Time, retentionDays int) time.Time {
	if retentionDays <= 0 {
		retentionDays = DefaultTraceRetentionDays
	}
	return now.Add(-time.Duration(retentionDays) * 24 * time.Hour)
}

// sweepExpired deletes agent_traces rows older than the retention window. Errors
// are logged, not fatal — a failed sweep must never disrupt trace ingestion.
func (w *MySQLWriter) sweepExpired() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cutoff := retentionCutoff(time.Now(), w.retentionDays)
	res, err := w.db.ExecContext(ctx,
		"DELETE FROM agent_traces WHERE created_at < $1", cutoff)
	if err != nil {
		w.logger.Printf("mysql_writer: retention sweep failed (cutoff=%s): %v",
			cutoff.Format(time.RFC3339), err)
		return
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		w.logger.Printf("mysql_writer: retention sweep removed %d rows older than %s",
			n, cutoff.Format(time.RFC3339))
	}
}

// insertBatch builds a single multi-VALUES INSERT for the batch. We rely on
// MySQL's INSERT IGNORE behavior on duplicate request_uuid (the unique key)
// so retries don't fail loudly; the engine reply path can re-enqueue
// without coordination.
// Column lists + per-row placeholders for the two INSERT shapes. The legacy 12-
// column form is the floor (always valid against a 0002 schema); the promoted form
// appends the 0004 outcome columns AFTER trace_json — so the order here must match
// rowFromTrace (base 12) followed by promotedColumnValues (the 7 extras).
const (
	legacyInsertCols = "(request_uuid, top_organization_id, organization_id, connection_id, " +
		"turn_index, created_at, status, intent, tool_count, cited_chunk_ids, " +
		"duration_ms, trace_json)"

	promotedInsertCols = "(request_uuid, top_organization_id, organization_id, connection_id, " +
		"turn_index, created_at, status, intent, tool_count, cited_chunk_ids, " +
		"duration_ms, trace_json, " +
		"terminated_by, abort_cause, error_class, resolution, route_status, " +
		"refusal_type, resolution_source)"

	promotedColCount = 19
)

func (w *MySQLWriter) insertBatch(batch []persistedTrace) (int, error) {
	if len(batch) == 0 {
		return 0, nil
	}
	cols := legacyInsertCols
	if w.promotedColumns {
		cols = promotedInsertCols
	}
	var placeholders strings.Builder
	args := make([]any, 0, len(batch)*promotedColCount)
	n := 0 // running $N counter across the whole multi-row VALUES list
	candidateCount := 0
	for _, p := range batch {
		row, err := rowFromTrace(p)
		if err != nil {
			w.malformedDropped.Add(1)
			w.logger.Printf("mysql_writer: skipping malformed trace_id=%s: %v",
				p.record.TraceID, err)
			continue
		}
		candidateCount++
		if w.promotedColumns {
			row = append(row, promotedColumnValues(p.record)...)
		}
		// Build this row's ($N,$N+1,...) group only after rowFromTrace succeeds, so a
		// skipped (malformed) record never desyncs placeholders from args.
		if placeholders.Len() > 0 {
			placeholders.WriteByte(',')
		}
		placeholders.WriteByte('(')
		for j := range row {
			if j > 0 {
				placeholders.WriteByte(',')
			}
			n++
			placeholders.WriteByte('$')
			placeholders.WriteString(strconv.Itoa(n))
		}
		placeholders.WriteByte(')')
		for _, v := range row {
			// lib/pq sends []byte as bytea, which won't insert into a jsonb column.
			// rowFromTrace returns cited_chunk_ids / trace_json as json.Marshal'd
			// []byte (kept that way for its unit tests); convert to string here so
			// PostgreSQL casts text→jsonb cleanly.
			if b, ok := v.([]byte); ok {
				args = append(args, string(b))
			} else {
				args = append(args, v)
			}
		}
	}
	if len(args) == 0 {
		return 0, nil
	}
	// ON CONFLICT DO NOTHING mirrors MySQL's INSERT IGNORE on the request_uuid
	// unique key so retried enqueues don't fail loudly.
	query := "INSERT INTO agent_traces " + cols + " VALUES " + placeholders.String() +
		" ON CONFLICT (request_uuid) DO NOTHING"

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := w.db.ExecContext(ctx, query, args...)
	return candidateCount, err
}

// promotedColumnValues projects the 0004 outcome columns from a finalized
// TraceRecord, in the same order promotedInsertCols lists them after trace_json.
// Each empty axis becomes SQL NULL (an answered turn that did not refuse has
// refusal_type = NULL, error_class = NULL), so a dashboard COUNT/GROUP BY over a
// column counts only the turns where that axis actually fired.
func promotedColumnValues(rec TraceRecord) []any {
	return []any{
		nullableStr(rec.Outcome.TerminatedBy),
		nullableStr(rec.Outcome.AbortCause),
		nullableStr(rec.Outcome.ErrorClass),
		nullableStr(rec.Outcome.Resolution),
		nullableStr(rec.IntentRouter.RouteStatus),
		nullableStr(rec.Retrieval.DeriveRefusalType()),
		nullableStr(rec.State.ResolutionSource),
	}
}

// nullableStr maps "" → SQL NULL and any non-empty string to itself, so empty
// attribution axes store as NULL rather than ” (keeps COUNT / GROUP BY clean).
func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// rowFromTrace projects a persistedTrace into the 12 column values for
// agent_traces. Defined as a free function so it stays trivially unit-
// testable without a live DB.
func rowFromTrace(p persistedTrace) ([]any, error) {
	rec := p.record
	createdAt := rec.Timestamp
	if createdAt == "" {
		createdAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	citedJSON, err := json.Marshal(rec.Retrieval.CitedChunkIDs)
	if err != nil {
		return nil, fmt.Errorf("marshal cited_chunk_ids: %w", err)
	}
	if len(rec.Retrieval.CitedChunkIDs) == 0 {
		// agent_traces.cited_chunk_ids is JSON NOT NULL — store [] not null
		citedJSON = []byte("[]")
	}
	traceJSON, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("marshal trace_json: %w", err)
	}
	return []any{
		rec.TraceID,
		p.tenant.TopOrgID,
		p.tenant.OrgID,
		p.tenant.ConnectionID,
		rec.TurnIndex,
		createdAt,
		statusFromTrace(rec),
		string(rec.IntentRouter.Intent),
		len(rec.ToolCalls),
		citedJSON,
		rec.Outcome.TotalLatencyMS,
		traceJSON,
	}, nil
}

// statusFromTrace collapses the trace record's terminal state into the
// agent_traces.status ENUM('success','blocked','error'). It now derives from the
// finalized outcome.terminated_by axis (FinalizeOutcome), which fixes the empty
// LLM reply hiding inside "success" while preserving the existing meanings of the
// three values:
//   - "blocked" = the engine deliberately stopped the turn: a hard-block,
//     rate-limit denial, OR a budget cap (token budget / ReAct round ceiling — the
//     token-budget path already reported "blocked" via its hard-block; the round
//     ceiling previously leaked into "success", which this un-masks).
//   - "error"   = the turn failed to complete for a non-policy reason: an LLM
//     error, timeout, empty reply (the dark-hole-within-the-dark-hole, previously
//     "success"), or a client disconnect.
//   - "success" = the turn delivered a normal answer.
//
// The precise terminated_by / abort_cause live in trace_json for queryability;
// when ops adds finer ENUM values (e.g. 'aborted') in Phase 1b, this collapse can
// widen.
//
// Legacy fallback: a record that never ran FinalizeOutcome (TerminatedBy=="")
// keeps the original trace-only inference, so older fixtures / un-finalized
// records are unaffected.
func statusFromTrace(rec TraceRecord) string {
	switch rec.Outcome.TerminatedBy {
	case TerminatedByBlocked, TerminatedByBudget:
		return "blocked"
	case TerminatedByDone:
		return "success"
	case TerminatedByError, TerminatedByEmptyReply, TerminatedByTimeout,
		TerminatedByUserCancel:
		return "error"
	}
	// Un-finalized record: original trace-only inference.
	if rec.EngineHardBlock.Hit {
		return "blocked"
	}
	if rec.RateLimit.Checked && !rec.RateLimit.Allowed {
		return "blocked"
	}
	return "success"
}

func defaultIfZero(v, dflt int) int {
	if v <= 0 {
		return dflt
	}
	return v
}

func defaultDurationIfZero(v, dflt time.Duration) time.Duration {
	if v <= 0 {
		return dflt
	}
	return v
}

func defaultLogger(l *log.Logger) *log.Logger {
	if l != nil {
		return l
	}
	return log.Default()
}
