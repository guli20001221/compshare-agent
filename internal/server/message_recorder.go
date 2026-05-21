package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/observability"

	// MySQL driver registered transitively by internal/observability, but
	// import the package here too so a future refactor that drops the
	// observability dep doesn't silently break our sql.Open.
	_ "github.com/go-sql-driver/mysql"
)

// MessageRecorder persists chat turns into agent_messages: one row per
// (user_message, assistant_reply) pair. Design mirrors MySQLWriter so
// behavior under outage / queue saturation is consistent across both
// sinks — never block Engine.Chat.
//
// Lifecycle:
//   - NewMessageRecorder opens + pings the DSN, starts a worker.
//   - Record buffers the row, returns immediately; drops + warns when
//     full (plan §8.4).
//   - Close drains under caller ctx, then closes the DB.
type MessageRecorder struct {
	db          *sql.DB
	queue       chan MessageEntry
	workerDone  chan struct{}
	batchSize   int
	flushPeriod time.Duration
	logger      *log.Logger
}

// MessageEntry is one row of agent_messages. ConnectionID + CreatedAt
// together identify a turn within its WS session (turns are processed
// serially, so created_at is strictly monotonic per connection);
// RequestUUID alone is the unique idempotency key (uk_request_uuid).
// Status mirrors the trace's terminal-state enum so dashboards can join
// on status without recomputing.
type MessageEntry struct {
	RequestUUID      string
	TopOrgID         int64
	OrgID            int64
	ConnectionID     string
	CreatedAt        time.Time
	UserMessage      string
	AssistantMessage string
	Status           string // "success" | "blocked" | "error"
	Model            string
	LatencyMS        int
}

// MessageRecorderOptions mirrors MySQLWriterOptions; defaults applied
// when fields are zero. QueueSize 1024 absorbs ~100s of bursts at the
// expected per-pod throughput of ~10/s.
type MessageRecorderOptions struct {
	QueueSize   int
	BatchSize   int
	FlushPeriod time.Duration
	Logger      *log.Logger
}

// NewMessageRecorder opens the MySQL DSN, pings to verify, and starts
// the background worker. Caller MUST invoke Close to drain on shutdown.
// Empty DSN returns an error — the same fail-fast contract MySQLWriter
// uses.
func NewMessageRecorder(dsn string, opts MessageRecorderOptions) (*MessageRecorder, error) {
	if dsn == "" {
		return nil, errors.New("message recorder: dsn is empty")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("message recorder open: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("message recorder ping: %w", err)
	}
	r := &MessageRecorder{
		db:          db,
		queue:       make(chan MessageEntry, defaultIntIfZero(opts.QueueSize, 1024)),
		workerDone:  make(chan struct{}),
		batchSize:   defaultIntIfZero(opts.BatchSize, 50),
		flushPeriod: defaultDurationIfZero(opts.FlushPeriod, time.Second),
		logger:      defaultLogger(opts.Logger),
	}
	go r.run()
	return r, nil
}

// Record buffers a row for asynchronous insert. Non-blocking: drops +
// warns on queue saturation to avoid stalling Engine.Chat on a slow DB.
// Returns nil even on drop — drop is a documented outcome, not an error.
func (r *MessageRecorder) Record(entry MessageEntry) error {
	if r == nil {
		return nil // no-op when recorder not configured
	}
	select {
	case r.queue <- entry:
		return nil
	default:
		r.logger.Printf("message_recorder: queue full, dropping request_uuid=%s tenant=%d/%d",
			entry.RequestUUID, entry.TopOrgID, entry.OrgID)
		return nil
	}
}

// Close drains pending rows under the caller's context, then closes the
// DB handle. Idempotent: calling twice returns the second close's
// errors but doesn't panic.
func (r *MessageRecorder) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	close(r.queue)
	select {
	case <-r.workerDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	return r.db.Close()
}

func (r *MessageRecorder) run() {
	defer close(r.workerDone)
	batch := make([]MessageEntry, 0, r.batchSize)
	tick := time.NewTicker(r.flushPeriod)
	defer tick.Stop()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := r.insertBatch(batch); err != nil {
			r.logger.Printf("message_recorder: batch insert failed (%d rows): %v",
				len(batch), err)
		}
		batch = batch[:0]
	}
	for {
		select {
		case entry, ok := <-r.queue:
			if !ok {
				flush()
				return
			}
			batch = append(batch, entry)
			if len(batch) >= r.batchSize {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

// insertBatch builds a single INSERT IGNORE for the batch. Like
// MySQLWriter, we rely on agent_messages.uk_request_uuid to de-dupe
// idempotent retries.
func (r *MessageRecorder) insertBatch(batch []MessageEntry) error {
	if len(batch) == 0 {
		return nil
	}
	const cols = "(request_uuid, top_organization_id, organization_id, connection_id, " +
		"created_at, user_message, assistant_message, status, model, latency_ms)"
	placeholders := make([]byte, 0, len(batch)*40)
	args := make([]any, 0, len(batch)*10)
	for i, e := range batch {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, "(?,?,?,?,?,?,?,?,?,?)"...)
		args = append(args,
			e.RequestUUID,
			e.TopOrgID,
			e.OrgID,
			e.ConnectionID,
			e.CreatedAt,
			e.UserMessage,
			e.AssistantMessage,
			e.Status,
			e.Model,
			e.LatencyMS,
		)
	}
	query := "INSERT IGNORE INTO agent_messages " + cols + " VALUES " + string(placeholders)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := r.db.ExecContext(ctx, query, args...)
	return err
}

// DeriveStatus collapses (chatErr, trace) into the agent_messages /
// agent_traces status enum. Used by handler.runChatTurn after each Chat
// call to produce the right status column.
//
// Priority (highest first):
//   - chatErr != nil → "error" (includes context cancel, internal panics)
//   - rate-limit denial → "blocked"
//   - engine hard-block hit → "blocked"
//   - otherwise → "success"
//
// Plan §7.7 spec.
func DeriveStatus(chatErr error, trace observability.TraceRecord) string {
	if chatErr != nil {
		if errors.Is(chatErr, governance.ErrRateLimited) {
			return "blocked"
		}
		return "error"
	}
	if trace.EngineHardBlock.Hit {
		return "blocked"
	}
	if trace.RateLimit.Checked && !trace.RateLimit.Allowed {
		return "blocked"
	}
	return "success"
}

func defaultIntIfZero(v, dflt int) int {
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
