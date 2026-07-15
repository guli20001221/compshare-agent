package turncoord

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type captureDurableTraceWriter struct {
	mu      sync.Mutex
	records []observability.TraceRecord
	tenant  []observability.TenantContext
}

func (w *captureDurableTraceWriter) Append(record observability.TraceRecord) error {
	return w.Enqueue(observability.TenantContext{}, record)
}

func (w *captureDurableTraceWriter) Enqueue(tenant observability.TenantContext, record observability.TraceRecord) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.records = append(w.records, record)
	w.tenant = append(w.tenant, tenant)
	return nil
}

func (*captureDurableTraceWriter) EmitStep(observability.StepTrace) error { return nil }
func (*captureDurableTraceWriter) Dir() string                            { return "" }
func (*captureDurableTraceWriter) Close(context.Context) error            { return nil }

func (w *captureDurableTraceWriter) snapshot() []observability.TraceRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]observability.TraceRecord(nil), w.records...)
}

func coordinatorWithTrace(t *testing.T, turns turnStore, sessions store.SessionStore, factory EngineFactory, replica string, writer observability.Writer) *Coordinator {
	t.Helper()
	c := NewCoordinator(turns, sessions, factory, Options{
		ReplicaID: replica, LeaseTTL: 900 * time.Millisecond, LeaseRenewInterval: 100 * time.Millisecond,
		InteractionPoll: 20 * time.Millisecond, ExecutionTimeout: 5 * time.Second,
		RecoveryScanInterval: time.Hour, MutatingToolsEnabled: true, TraceWriter: writer,
	})
	t.Cleanup(c.Close)
	return c
}

func traceTestDSN(t *testing.T, db *sql.DB) string {
	t.Helper()
	var schema string
	require.NoError(t, db.QueryRow(`SELECT current_schema()`).Scan(&schema))
	u, err := url.Parse(os.Getenv("COMPSHARE_TEST_MYSQL_DSN"))
	require.NoError(t, err)
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	return u.String()
}

func TestCoordinator_DurableCommitPersistsAttemptTraceToPostgres(t *testing.T) {
	db := openActionJournalTestDB(t)
	ctx := context.Background()
	owner := store.Owner{TopOrganizationID: 97001, OrganizationID: 97002}
	sessions := store.NewSessionStore(db)
	session, err := sessions.Create(ctx, owner, nil, json.RawMessage(`{"schema_version":"1.0"}`))
	require.NoError(t, err)
	writer, err := observability.NewMySQLWriter(traceTestDSN(t, db), observability.MySQLWriterOptions{
		BatchSize: 1, FlushPeriod: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Close(context.Background()) })
	factory := &coordinatorFactory{}
	coordinator := coordinatorWithTrace(t, store.NewPostgresTurnStore(db), sessions, EngineFactoryFunc(factory.New), "trace-pg", writer)

	submission, err := coordinator.Submit(ctx, SubmitInput{
		Owner: owner, SessionID: session.ID, ClientTurnID: "trace-commit", Message: "继续刚才的操作",
	}, nil)
	require.NoError(t, err)
	committed := waitTurnStatus(t, store.NewPostgresTurnStore(db), owner, submission.Turn.ID, store.TurnStatusCommitted)
	require.NotNil(t, committed.CommittedLeaseEpoch)

	var traceJSON []byte
	require.Eventually(t, func() bool {
		return db.QueryRow(`SELECT trace_json FROM agent_traces WHERE request_uuid = $1`, traceAttemptID(submission.Turn.ID, *committed.CommittedLeaseEpoch)).Scan(&traceJSON) == nil
	}, 5*time.Second, 20*time.Millisecond)
	var trace observability.TraceRecord
	require.NoError(t, json.Unmarshal(traceJSON, &trace))
	assert.Equal(t, submission.Turn.ID, trace.TurnID)
	assert.Equal(t, traceAttemptID(submission.Turn.ID, *committed.CommittedLeaseEpoch), trace.TraceID)
	assert.Equal(t, submission.Turn.Sequence, trace.Continuity.TurnSequence)
	assert.Equal(t, *committed.CommittedLeaseEpoch, trace.Continuity.LeaseEpoch)
	assert.Equal(t, "valid", trace.Continuity.EnvelopeParseOutcome)
	assert.Equal(t, "valid", trace.Continuity.ContextParseOutcome)
	assert.Equal(t, "committed", trace.Continuity.CommitOutcome)
	assert.True(t, trace.Continuity.SessionIdentityMatch)
	assert.Equal(t, observability.CompletionClassAgent, trace.Completion.Class)
	assert.Equal(t, 1, trace.Context.LoopMessages)
}

func TestCoordinator_RetryEpochsProduceDistinctAttemptTraces(t *testing.T) {
	db := openActionJournalTestDB(t)
	ctx := context.Background()
	owner := store.Owner{TopOrganizationID: 97101, OrganizationID: 97102}
	sessions := store.NewSessionStore(db)
	session, err := sessions.Create(ctx, owner, nil, nil)
	require.NoError(t, err)
	turns := store.NewPostgresTurnStore(db)
	writer := &captureDurableTraceWriter{}
	factory := &coordinatorFactory{newChat: func(call int) func(context.Context, func(engine.StepEvent), engine.ChatOptions) (string, error) {
		return func(context.Context, func(engine.StepEvent), engine.ChatOptions) (string, error) {
			if call == 1 {
				return "", assert.AnError
			}
			return "recovered answer", nil
		}
	}}
	coordinator := coordinatorWithTrace(t, turns, sessions, EngineFactoryFunc(factory.New), "trace-retry", writer)
	submission, err := coordinator.Submit(ctx, SubmitInput{Owner: owner, SessionID: session.ID, ClientTurnID: "trace-retry", Message: "继续"}, nil)
	require.NoError(t, err)
	waitTurnRetryCount(t, turns, owner, submission.Turn.ID, store.TurnStatusFailedRetryable, 1)
	forceCoordinatorRetryDue(t, db, submission.Turn.ID)
	require.NoError(t, coordinator.RecoverTurn(ctx, owner, submission.Turn.ID))
	waitTurnStatus(t, turns, owner, submission.Turn.ID, store.TurnStatusCommitted)
	require.Eventually(t, func() bool { return len(writer.snapshot()) == 2 }, 3*time.Second, 20*time.Millisecond)

	records := writer.snapshot()
	assert.Equal(t, submission.Turn.ID, records[0].TurnID)
	assert.Equal(t, submission.Turn.ID, records[1].TurnID)
	assert.NotEqual(t, records[0].TraceID, records[1].TraceID)
	assert.Equal(t, int64(1), records[0].Continuity.LeaseEpoch)
	assert.Equal(t, int64(2), records[1].Continuity.LeaseEpoch)
	assert.Equal(t, string(store.TurnStatusFailedRetryable), records[0].Continuity.CommitOutcome)
	assert.Equal(t, "execution_failed", records[0].Continuity.CommitReason)
	assert.Equal(t, "committed", records[1].Continuity.CommitOutcome)
	assert.True(t, records[1].Continuity.RecoveryAttempt)
}

type doubleAckLostTraceStore struct {
	*store.PostgresTurnStore
	commitLost    atomic.Bool
	reconcileLost atomic.Bool
}

func (s *doubleAckLostTraceStore) CommitTurn(ctx context.Context, owner store.Owner, in store.CommitTurnInput) (store.Turn, error) {
	committed, err := s.PostgresTurnStore.CommitTurn(ctx, owner, in)
	if err != nil {
		return committed, err
	}
	if s.commitLost.CompareAndSwap(false, true) {
		return store.Turn{}, assert.AnError
	}
	return committed, nil
}

func (s *doubleAckLostTraceStore) ReconcileCommit(context.Context, store.Owner, store.CommitTurnInput) (store.Turn, error) {
	if s.reconcileLost.CompareAndSwap(false, true) {
		return store.Turn{}, assert.AnError
	}
	return store.Turn{}, assert.AnError
}

func TestCoordinator_LateCommittedSettlementTraceIsInternallyConsistent(t *testing.T) {
	db := openActionJournalTestDB(t)
	ctx := context.Background()
	owner := store.Owner{TopOrganizationID: 97151, OrganizationID: 97152}
	sessions := store.NewSessionStore(db)
	session, err := sessions.Create(ctx, owner, nil, nil)
	require.NoError(t, err)
	base := store.NewPostgresTurnStore(db)
	turns := &doubleAckLostTraceStore{PostgresTurnStore: base}
	writer := &captureDurableTraceWriter{}
	coordinator := coordinatorWithTrace(t, turns, sessions, EngineFactoryFunc((&coordinatorFactory{}).New), "trace-late-commit", writer)
	submission, err := coordinator.Submit(ctx, SubmitInput{Owner: owner, SessionID: session.ID, ClientTurnID: "late-commit", Message: "save once"}, nil)
	require.NoError(t, err)
	waitTurnStatus(t, base, owner, submission.Turn.ID, store.TurnStatusCommitted)
	require.Eventually(t, func() bool { return len(writer.snapshot()) == 1 }, 3*time.Second, 20*time.Millisecond)
	record := writer.snapshot()[0]
	assert.Equal(t, "late_reconciled_committed", record.Continuity.CommitOutcome)
	assert.Empty(t, record.Continuity.CommitReason)
	assert.False(t, record.EngineHardBlock.Hit)
	assert.Equal(t, observability.TerminatedByDone, record.Outcome.TerminatedBy)
}

func TestCoordinator_MalformedEnvelopeWritesZeroModelTrace(t *testing.T) {
	db := openActionJournalTestDB(t)
	ctx := context.Background()
	owner := store.Owner{TopOrganizationID: 97201, OrganizationID: 97202}
	sessions := store.NewSessionStore(db)
	session, err := sessions.Create(ctx, owner, nil, nil)
	require.NoError(t, err)
	turns := store.NewPostgresTurnStore(db)
	broken, _, err := turns.AcceptTurn(ctx, owner, store.AcceptTurnInput{
		SessionID: session.ID, ClientTurnID: "trace-broken", RequestHash: store.HashTurnRequest("trace-broken"),
		UserContent: "continue", ExecutionEnvelope: json.RawMessage(`{"version":2}`),
	})
	require.NoError(t, err)
	writer := &captureDurableTraceWriter{}
	factory := &coordinatorFactory{}
	_ = coordinatorWithTrace(t, turns, sessions, EngineFactoryFunc(factory.New), "trace-broken", writer)
	waitTurnStatus(t, turns, owner, broken.ID, store.TurnStatusFailedFinal)
	require.Eventually(t, func() bool { return len(writer.snapshot()) == 1 }, 3*time.Second, 20*time.Millisecond)
	record := writer.snapshot()[0]
	assert.Equal(t, "execution_envelope_invalid", record.Continuity.EnvelopeParseOutcome)
	assert.Equal(t, "not_read", record.Continuity.ContextParseOutcome)
	assert.Equal(t, string(store.TurnStatusFailedFinal), record.Continuity.CommitOutcome)
	assert.Equal(t, "execution_envelope_invalid", record.Continuity.CommitReason)
	assert.Zero(t, record.Completion.ModelCalls)
	factory.mu.Lock()
	assert.Zero(t, factory.calls)
	factory.mu.Unlock()
}

type nonTraceableEngine struct {
	state engine.SessionState
	ver   int
	calls *atomic.Int32
}

func (e *nonTraceableEngine) SetSessionState(state engine.SessionState, version int) {
	e.state, e.ver = state, version
}
func (*nonTraceableEngine) SetContinuityAdvisories(engine.ContinuityAdvisories) {}
func (e *nonTraceableEngine) SessionStateSnapshot() (engine.SessionState, int, bool) {
	return e.state, e.ver, true
}
func (e *nonTraceableEngine) ChatWithOptions(context.Context, string, func(engine.StepEvent), engine.ChatOptions) (string, error) {
	e.calls.Add(1)
	return "must not run", nil
}

func TestCoordinator_TracingRejectsNonTraceableEngineBeforeModel(t *testing.T) {
	db := openActionJournalTestDB(t)
	ctx := context.Background()
	owner := store.Owner{TopOrganizationID: 97301, OrganizationID: 97302}
	sessions := store.NewSessionStore(db)
	session, err := sessions.Create(ctx, owner, nil, nil)
	require.NoError(t, err)
	turns := store.NewPostgresTurnStore(db)
	writer := &captureDurableTraceWriter{}
	var calls atomic.Int32
	factory := EngineFactoryFunc(func(context.Context, store.Owner, string, engine.SessionOptions) (TurnEngine, error) {
		return &nonTraceableEngine{calls: &calls}, nil
	})
	coordinator := coordinatorWithTrace(t, turns, sessions, factory, "trace-contract", writer)
	submission, err := coordinator.Submit(ctx, SubmitInput{Owner: owner, SessionID: session.ID, ClientTurnID: "trace-contract", Message: "hello"}, nil)
	require.NoError(t, err)
	waitTurnStatus(t, turns, owner, submission.Turn.ID, store.TurnStatusFailedFinal)
	require.Eventually(t, func() bool { return len(writer.snapshot()) == 1 }, 3*time.Second, 20*time.Millisecond)
	assert.Zero(t, calls.Load())
	record := writer.snapshot()[0]
	assert.Equal(t, "trace_engine_unsupported", record.Continuity.CommitReason)
	assert.Equal(t, string(store.TurnStatusFailedFinal), record.Continuity.CommitOutcome)
}

func TestBoundedContinuityReasonRejectsFreeText(t *testing.T) {
	secret := "database failed password=hunter2"
	got := boundedContinuityReason(secret)
	assert.Equal(t, "other", got)
	assert.NotContains(t, got, "hunter2")
	assert.Equal(t, "turn_not_saved", boundedContinuityReason("turn_not_saved"))
}

func TestTraceAttemptIDKeepsLogicalTurnAndLeaseEpoch(t *testing.T) {
	assert.Equal(t, "turn-123:e7", traceAttemptID("turn-123", 7))
	assert.True(t, strings.HasPrefix(traceAttemptID("turn-123", 7), "turn-123"))
}
