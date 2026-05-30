package observability

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	// MySQL driver registered via blank import. The Server bootstrap (A3) is
	// responsible for verifying connectivity at startup so callers of this
	// package never see driver-registration failures.
	_ "github.com/go-sql-driver/mysql"
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
	db          *sql.DB
	queue       chan persistedTrace
	workerDone  chan struct{}
	batchSize   int
	flushPeriod time.Duration
	logger      *log.Logger
}

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
	QueueSize   int           // default 1024
	BatchSize   int           // default 50
	FlushPeriod time.Duration // default 1s
	Logger      *log.Logger   // default log.Default()
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
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("mysql writer open: %w", err)
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
		db:          db,
		queue:       make(chan persistedTrace, defaultIfZero(opts.QueueSize, 1024)),
		workerDone:  make(chan struct{}),
		batchSize:   defaultIfZero(opts.BatchSize, 50),
		flushPeriod: defaultDurationIfZero(opts.FlushPeriod, time.Second),
		logger:      defaultLogger(opts.Logger),
	}
	go w.run()
	return w, nil
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
	select {
	case w.queue <- persistedTrace{tenant: tenant, record: record}:
		return nil
	default:
		w.logger.Printf("mysql_writer: queue full, dropping trace_id=%s tenant=%d/%d",
			record.TraceID, tenant.TopOrgID, tenant.OrgID)
		return nil
	}
}

// EmitStep is a no-op on the MySQL sink. Agent-tier saga steps are accumulated
// in the per-turn recorder (chatTraceRecorder.EmitStep) and persisted inside
// trace_json with the single Enqueue at turn Finish — never a per-step INSERT
// (that would collide uk_request_uuid: one agent_traces row per turn). See the
// Writer.EmitStep interface doc.
func (w *MySQLWriter) EmitStep(StepTrace) error { return nil }

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
		if err := w.insertBatch(batch); err != nil {
			w.logger.Printf("mysql_writer: batch insert failed (%d records): %v",
				len(batch), err)
		}
		batch = batch[:0]
	}

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
		}
	}
}

// insertBatch builds a single multi-VALUES INSERT for the batch. We rely on
// MySQL's INSERT IGNORE behavior on duplicate request_uuid (the unique key)
// so retries don't fail loudly; the engine reply path can re-enqueue
// without coordination.
func (w *MySQLWriter) insertBatch(batch []persistedTrace) error {
	if len(batch) == 0 {
		return nil
	}
	const cols = "(request_uuid, top_organization_id, organization_id, connection_id, " +
		"turn_index, created_at, status, intent, tool_count, cited_chunk_ids, " +
		"duration_ms, trace_json)"
	placeholders := make([]byte, 0, len(batch)*40)
	args := make([]any, 0, len(batch)*12)
	for i, p := range batch {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, "(?,?,?,?,?,?,?,?,?,?,?,?)"...)
		row, err := rowFromTrace(p)
		if err != nil {
			w.logger.Printf("mysql_writer: skipping malformed trace_id=%s: %v",
				p.record.TraceID, err)
			continue
		}
		args = append(args, row...)
	}
	if len(args) == 0 {
		return nil
	}
	query := "INSERT IGNORE INTO agent_traces " + cols + " VALUES " + string(placeholders)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := w.db.ExecContext(ctx, query, args...)
	return err
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
		string(rec.Planner.Intent),
		len(rec.ToolCalls),
		citedJSON,
		rec.Outcome.TotalLatencyMS,
		traceJSON,
	}, nil
}

// statusFromTrace collapses the trace record's terminal state into the
// agent_traces.status enum. Mirrors plan §7.7 DeriveStatus.
//   - "blocked": engine hard-block fired OR rate-limit denial.
//   - "error":   reserved for caller (chatErr != nil); statusFromTrace can't
//     distinguish error from success without the caller's error value, so
//     callers SHOULD pre-set rec.Outcome.AttemptedHallucinatedCount > 0 or
//     use a wrapper. For pure-trace inference the default is "success".
//
// The server WS handler uses the richer server.DeriveStatus(chatErr,trace)
// helper (lands in A5/PR5); this internal version covers the file/MySQL
// boundary when only the trace is available.
func statusFromTrace(rec TraceRecord) string {
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
