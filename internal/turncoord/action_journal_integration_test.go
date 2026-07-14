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
	for _, name := range []string{"0001_init.sql", "0003_add_session_context_version.sql", "0005_create_turn_execution.sql", "0006_create_turn_protocol.sql"} {
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
	first, err := NewActionJournal(turns, owner, leaseA).Execute(ctx, "StopCompShareInstance", argsA, func(context.Context, string, map[string]any) (map[string]any, error) {
		calls++
		return map[string]any{"RetCode": 0, "RequestId": "req-1"}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, "req-1", first["RequestId"])

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
	assert.Equal(t, "req-1", replayed["RequestId"])
}

func TestActionJournal_DefiniteUpstreamFailureReplays(t *testing.T) {
	db := openActionJournalTestDB(t)
	ctx, owner, turns, leaseA := newJournalTurn(t, db)
	calls := 0
	jA := NewActionJournal(turns, owner, leaseA)
	_, err := jA.Execute(ctx, "StopCompShareInstance", map[string]any{"UHostId": "uhost-1"}, func(context.Context, string, map[string]any) (map[string]any, error) {
		calls++
		return nil, tools.NewUpstreamAPIError(230, "capacity")
	})
	var first *tools.UpstreamAPIError
	require.ErrorAs(t, err, &first)
	leaseB := takeoverJournalLease(t, db, turns, ctx, owner, leaseA)
	_, err = NewActionJournal(turns, owner, leaseB).Execute(ctx, "StopCompShareInstance", map[string]any{"UHostId": "uhost-1"}, func(context.Context, string, map[string]any) (map[string]any, error) {
		calls++
		return nil, nil
	})
	var replayed *tools.UpstreamAPIError
	require.ErrorAs(t, err, &replayed)
	assert.Equal(t, 230, replayed.Code)
	assert.Equal(t, "capacity", replayed.Message)
	assert.Equal(t, 1, calls)
}
