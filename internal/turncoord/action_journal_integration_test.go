package turncoord

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openActionJournalTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("COMPSHARE_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("COMPSHARE_TEST_MYSQL_DSN not set")
	}
	if strings.Contains(dsn, "117.50.198.43") {
		t.Fatal("refusing production PostgreSQL")
	}
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	require.NoError(t, admin.Ping())
	schema := "journal_it_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
		_ = admin.Close()
	})
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("postgres", u.String())
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	t.Cleanup(func() { _ = db.Close() })
	for _, name := range []string{"0001_init.sql", "0003_add_session_context_version.sql", "0005_create_turn_execution.sql", "0006_create_turn_protocol.sql", "0007_add_turn_recovery_context.sql", "0008_add_turn_retry_policy.sql"} {
		data, readErr := os.ReadFile(filepath.Join("..", "..", "deploy", "migrations", name))
		require.NoError(t, readErr)
		_, execErr := db.Exec(string(data))
		require.NoError(t, execErr, name)
	}
	return db
}

func newJournalTurn(t *testing.T, db *sql.DB) (context.Context, store.Owner, *store.PostgresTurnStore, store.ConversationLease) {
	t.Helper()
	ctx := context.Background()
	owner := store.Owner{TopOrganizationID: 73001, OrganizationID: 73002}
	session, err := store.NewSessionStore(db).Create(ctx, owner, nil, json.RawMessage(`{"schema_version":"1.0"}`))
	require.NoError(t, err)
	turns := store.NewPostgresTurnStore(db)
	turn, _, err := turns.AcceptTurn(ctx, owner, store.AcceptTurnInput{
		SessionID: session.ID, ClientTurnID: "journal-turn", RequestHash: store.HashTurnRequest("journal-turn"), UserContent: "do it",
	})
	require.NoError(t, err)
	lease, err := turns.AcquireConversationLease(ctx, owner, session.ID, turn.ID, "replica-a", time.Minute)
	require.NoError(t, err)
	return ctx, owner, turns, lease
}

func takeoverJournalLease(t *testing.T, db *sql.DB, turns *store.PostgresTurnStore, ctx context.Context, owner store.Owner, old store.ConversationLease) store.ConversationLease {
	t.Helper()
	_, err := db.Exec(`UPDATE conversation_leases SET lease_until = NOW() - INTERVAL '1 second' WHERE session_id = $1`, old.SessionID)
	require.NoError(t, err)
	next, err := turns.AcquireConversationLease(ctx, owner, old.SessionID, old.TurnID, "replica-b", time.Minute)
	require.NoError(t, err)
	return next
}

func TestActionJournal_ReserveBeforeStartCrashIsSafelyClaimedOnce(t *testing.T) {
	db := openActionJournalTestDB(t)
	ctx, owner, turns, leaseA := newJournalTurn(t, db)
	_, _, err := turns.ReserveAction(ctx, owner, leaseA, store.ReserveActionInput{
		Index: 0, ActionName: "StopCompShareInstance", ArgsHash: canonicalActionArgsHash(map[string]any{"UHostId": "uhost-1"}),
	})
	require.NoError(t, err)
	leaseB := takeoverJournalLease(t, db, turns, ctx, owner, leaseA)
	calls := 0
	result, err := NewActionJournal(turns, owner, leaseB).Execute(ctx, "StopCompShareInstance", map[string]any{"UHostId": "uhost-1"}, func(context.Context, string, map[string]any) (map[string]any, error) {
		calls++
		return map[string]any{"RetCode": 0}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	assert.EqualValues(t, 0, result["RetCode"])
}

func TestActionJournal_StartedCrashTakeoverNeverCallsUpstream(t *testing.T) {
	db := openActionJournalTestDB(t)
	ctx, owner, turns, leaseA := newJournalTurn(t, db)
	action, _, err := turns.ReserveAction(ctx, owner, leaseA, store.ReserveActionInput{
		Index: 0, ActionName: "StopCompShareInstance", ArgsHash: canonicalActionArgsHash(map[string]any{"UHostId": "uhost-1"}),
	})
	require.NoError(t, err)
	_, err = turns.StartAction(ctx, owner, leaseA, action.ExecutionToken)
	require.NoError(t, err)
	leaseB := takeoverJournalLease(t, db, turns, ctx, owner, leaseA)
	calls := 0
	_, err = NewActionJournal(turns, owner, leaseB).Execute(ctx, "StopCompShareInstance", map[string]any{"UHostId": "uhost-1"}, func(context.Context, string, map[string]any) (map[string]any, error) {
		calls++
		return nil, nil
	})
	require.ErrorIs(t, err, tools.ErrActionOutcomeUncertain)
	assert.Zero(t, calls)
}

func TestActionJournal_KnownSuccessReplaysAfterAnswerCommitFailure(t *testing.T) {
	db := openActionJournalTestDB(t)
	ctx, owner, turns, leaseA := newJournalTurn(t, db)
	calls := 0
	argsA := map[string]any{"Name": "worker", "UHostId": "uhost-1"}
	journalA := NewActionJournal(turns, owner, leaseA)
	first, err := journalA.Execute(ctx, "StopCompShareInstance", argsA, func(context.Context, string, map[string]any) (map[string]any, error) {
		calls++
		return map[string]any{"RetCode": 0, "request_uuid": "req-1"}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, "req-1", first["request_uuid"])
	_, err = journalA.Execute(ctx, "StopCompShareInstance", map[string]any{"UHostId": "uhost-1", "Name": "worker"}, func(context.Context, string, map[string]any) (map[string]any, error) {
		calls++
		return nil, errors.New("same semantic mutation must not run twice")
	})
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	var persistedRequestID *string
	require.NoError(t, db.QueryRow(`SELECT upstream_request_id FROM turn_actions WHERE turn_id = $1 AND action_index = 0`, leaseA.TurnID).Scan(&persistedRequestID))
	require.NotNil(t, persistedRequestID)
	assert.Equal(t, "req-1", *persistedRequestID)

	// The answer/state commit never happens. A new executor repeats the same
	// logical action with a differently ordered map and must replay the result.
	leaseB := takeoverJournalLease(t, db, turns, ctx, owner, leaseA)
	argsB := map[string]any{"UHostId": "uhost-1", "Name": "worker"}
	replayed, err := NewActionJournal(turns, owner, leaseB).Execute(ctx, "StopCompShareInstance", argsB, func(context.Context, string, map[string]any) (map[string]any, error) {
		calls++
		return nil, errors.New("must not call")
	})
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	assert.Equal(t, "req-1", replayed["request_uuid"])
}

func TestActionJournal_KnownSuccessCanBeConsumedAsAdvisoryWithoutReissuingWrite(t *testing.T) {
	db := openActionJournalTestDB(t)
	ctx, owner, turns, leaseA := newJournalTurn(t, db)
	upstreamCalls := 0
	journalA := NewActionJournal(turns, owner, leaseA)
	_, err := journalA.Execute(ctx, "StopCompShareInstance", map[string]any{
		"UHostId": "uhost-advisory", "Region": "cn-bj2", "Password": "must-not-persist-in-hint",
	}, func(context.Context, string, map[string]any) (map[string]any, error) {
		upstreamCalls++
		return map[string]any{"RetCode": 0, "request_uuid": "raw-result-must-not-enter-advisory"}, nil
	})
	require.NoError(t, err)
	leaseB := takeoverJournalLease(t, db, turns, ctx, owner, leaseA)
	journalB := NewActionJournal(turns, owner, leaseB)
	advisories, err := journalB.RestoredActionAdvisory(ctx)
	require.NoError(t, err)
	require.Len(t, advisories, 1)
	assert.Equal(t, "StopCompShareInstance", advisories[0].ActionName)
	assert.Equal(t, "succeeded", advisories[0].Outcome)
	assert.Contains(t, string(advisories[0].ContextHint), "uhost-advisory")
	assert.Contains(t, string(advisories[0].ContextHint), "cn-bj2")
	assert.NotContains(t, string(advisories[0].ContextHint), "Password")
	assert.NotContains(t, string(advisories[0].ContextHint), "raw-result-must-not-enter-advisory")
	require.NoError(t, journalB.VerifyComplete(ctx), "the known success is consumed by the advisory")

	_, err = journalB.Execute(ctx, "DeleteCompShareInstance", map[string]any{"UHostId": "uhost-advisory"}, func(context.Context, string, map[string]any) (map[string]any, error) {
		upstreamCalls++
		return map[string]any{"RetCode": 0}, nil
	})
	require.ErrorIs(t, err, tools.ErrActionOutcomeUncertain)
	assert.Equal(t, 1, upstreamCalls, "advisory consumption must not open an extra write slot")
}

func TestActionJournal_UpstreamBusinessErrorIsAmbiguousAndNeverReissued(t *testing.T) {
	db := openActionJournalTestDB(t)
	ctx, owner, turns, leaseA := newJournalTurn(t, db)
	calls := 0
	jA := NewActionJournal(turns, owner, leaseA)
	_, err := jA.Execute(ctx, "StopCompShareInstance", map[string]any{"UHostId": "uhost-1"}, func(context.Context, string, map[string]any) (map[string]any, error) {
		calls++
		return nil, tools.NewUpstreamAPIError(230, "capacity")
	})
	require.ErrorIs(t, err, tools.ErrActionOutcomeUncertain)
	require.Error(t, jA.Err())
	leaseB := takeoverJournalLease(t, db, turns, ctx, owner, leaseA)
	_, err = NewActionJournal(turns, owner, leaseB).Execute(ctx, "StopCompShareInstance", map[string]any{"UHostId": "uhost-1"}, func(context.Context, string, map[string]any) (map[string]any, error) {
		calls++
		return nil, nil
	})
	require.ErrorIs(t, err, tools.ErrActionOutcomeUncertain)
	assert.Equal(t, 1, calls)
}

func TestActionJournal_TakeoverReplayOnlyRejectsDifferentOrOmittedActions(t *testing.T) {
	t.Run("different action", func(t *testing.T) {
		db := openActionJournalTestDB(t)
		ctx, owner, turns, leaseA := newJournalTurn(t, db)
		_, err := NewActionJournal(turns, owner, leaseA).Execute(ctx, "ExternalWrite", map[string]any{"target": "one"}, func(context.Context, string, map[string]any) (map[string]any, error) {
			return map[string]any{"RetCode": 0}, nil
		})
		require.NoError(t, err)
		leaseB := takeoverJournalLease(t, db, turns, ctx, owner, leaseA)
		calls := 0
		journalB := NewActionJournal(turns, owner, leaseB)
		_, err = journalB.Execute(ctx, "ExternalWrite", map[string]any{"target": "two"}, func(context.Context, string, map[string]any) (map[string]any, error) {
			calls++
			return map[string]any{"RetCode": 0}, nil
		})
		require.ErrorIs(t, err, tools.ErrActionOutcomeUncertain)
		assert.Zero(t, calls)
		require.Error(t, journalB.VerifyComplete(ctx))
	})

	t.Run("omitted action", func(t *testing.T) {
		db := openActionJournalTestDB(t)
		ctx, owner, turns, leaseA := newJournalTurn(t, db)
		_, err := NewActionJournal(turns, owner, leaseA).Execute(ctx, "ExternalWrite", map[string]any{"target": "one"}, func(context.Context, string, map[string]any) (map[string]any, error) {
			return map[string]any{"RetCode": 0}, nil
		})
		require.NoError(t, err)
		leaseB := takeoverJournalLease(t, db, turns, ctx, owner, leaseA)
		require.ErrorIs(t, NewActionJournal(turns, owner, leaseB).VerifyComplete(ctx), tools.ErrActionOutcomeUncertain)
	})
}

func TestActionJournal_ReplayOnlyHandlesSemanticDuplicateIndexGaps(t *testing.T) {
	db := openActionJournalTestDB(t)
	ctx, owner, turns, leaseA := newJournalTurn(t, db)
	journalA := NewActionJournal(turns, owner, leaseA)
	upstreamCalls := 0
	call := func(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
		upstreamCalls++
		return map[string]any{"RetCode": 0, "action": action}, nil
	}
	argsX := map[string]any{"target": "x"}
	argsY := map[string]any{"target": "y"}
	_, err := journalA.Execute(ctx, "WriteX", argsX, call)
	require.NoError(t, err)
	_, err = journalA.Execute(ctx, "WriteX", argsX, call)
	require.NoError(t, err)
	_, err = journalA.Execute(ctx, "WriteY", argsY, call)
	require.NoError(t, err)
	assert.Equal(t, 2, upstreamCalls, "semantic duplicate X must already replay in the first attempt")

	leaseB := takeoverJournalLease(t, db, turns, ctx, owner, leaseA)
	journalB := NewActionJournal(turns, owner, leaseB)
	noCall := func(context.Context, string, map[string]any) (map[string]any, error) {
		upstreamCalls++
		return nil, errors.New("takeover must only replay")
	}
	_, err = journalB.Execute(ctx, "WriteX", argsX, noCall)
	require.NoError(t, err)
	_, err = journalB.Execute(ctx, "WriteX", argsX, noCall)
	require.NoError(t, err)
	_, err = journalB.Execute(ctx, "WriteY", argsY, noCall)
	require.NoError(t, err)
	require.NoError(t, journalB.VerifyComplete(ctx))
	assert.Equal(t, 2, upstreamCalls)
}
