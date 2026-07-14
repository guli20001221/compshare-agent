package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openIsolatedTurnTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("COMPSHARE_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("COMPSHARE_TEST_MYSQL_DSN not set — skipping real PostgreSQL turn-store integration test")
	}
	if strings.Contains(dsn, "117.50.198.43") {
		t.Fatal("refusing to run the integration test against the production PostgreSQL host")
	}

	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	require.NoError(t, admin.Ping())
	schema := "turn_it_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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

	for _, name := range []string{
		"0001_init.sql",
		"0003_add_session_context_version.sql",
		"0005_create_turn_execution.sql",
		"0006_create_turn_protocol.sql",
		"0007_add_turn_recovery_context.sql",
		"0008_add_turn_retry_policy.sql",
	} {
		data, readErr := os.ReadFile(filepath.Join("..", "..", "deploy", "migrations", name))
		require.NoError(t, readErr)
		_, execErr := db.Exec(string(data))
		require.NoError(t, execErr, "apply %s", name)
	}
	require.NoError(t, VerifySchema(context.Background(), db))
	return db
}

// insertLegacyAcceptedTurnForTest recreates a turn that an older release could
// have admitted behind another non-terminal turn. New requests must never use
// this path; it exists only to keep takeover and upgrade compatibility covered.
func insertLegacyAcceptedTurnForTest(t *testing.T, db *sql.DB, owner Owner, in AcceptTurnInput) Turn {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer tx.Rollback()

	contextVersion, err := lockSession(ctx, tx, owner, in.SessionID)
	require.NoError(t, err)
	turnSequence, err := nextTurnSequence(ctx, tx, in.SessionID)
	require.NoError(t, err)
	envelope, err := canonicalOptionalObject(in.ExecutionEnvelope)
	require.NoError(t, err)
	turnID := uuid.NewString()
	userMessageID := uuid.NewString()
	assistantMessageID := uuid.NewString()
	turn, err := insertTurn(ctx, tx, owner, in, turnID, userMessageID, assistantMessageID, contextVersion, turnSequence, envelope)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
INSERT INTO messages
  (id, session_id, request_uuid, role, content, status, model, metadata, turn_id, turn_role)
VALUES
  ($1, $2, $3, 'user', $4, 'pending', NULL, $5, $6, 'user'),
  ($7, $2, $3, 'assistant', '', 'pending', $8, NULL, $6, 'assistant')
`, userMessageID, in.SessionID, nullableRequestUUID(in.RequestUUID), in.UserContent,
		nullableJSON(in.UserMetadata), turnID, assistantMessageID, nullableStringPtr(in.AssistantModel))
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	return turn
}

func TestPostgresTurnStore_Integration(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 71001, OrganizationID: 71002}
	otherOwner := Owner{TopOrganizationID: 71001, OrganizationID: 71999}
	session, err := NewSessionStore(db).Create(ctx, owner, nil, json.RawMessage(`{"schema_version":"1.0"}`))
	require.NoError(t, err)

	turns := NewPostgresTurnStore(db)
	requestHash := HashTurnRequest("hello")
	turn, created, err := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
		SessionID:    session.ID,
		ClientTurnID: "client-turn-1",
		RequestHash:  requestHash,
		UserContent:  "hello",
	})
	require.NoError(t, err)
	require.True(t, created)
	assert.Equal(t, TurnStatusAccepted, turn.Status)
	assert.Equal(t, 0, turn.BaseContextVersion)

	same, created, err := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
		SessionID:    session.ID,
		ClientTurnID: "client-turn-1",
		RequestHash:  requestHash,
		UserContent:  "hello",
	})
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, turn.ID, same.ID)
	assert.Equal(t, turn.UserMessageID, same.UserMessageID)

	_, _, err = turns.AcceptTurn(ctx, owner, AcceptTurnInput{
		SessionID:    session.ID,
		ClientTurnID: "client-turn-1",
		RequestHash:  HashTurnRequest("different"),
		UserContent:  "different",
	})
	require.ErrorIs(t, err, ErrIdempotencyConflict)
	_, err = turns.GetTurn(ctx, otherOwner, turn.ID)
	require.ErrorIs(t, err, ErrTurnNotFound)

	var messageCount int
	require.NoError(t, db.QueryRow(`SELECT message_count FROM sessions WHERE id = $1`, session.ID).Scan(&messageCount))
	assert.Zero(t, messageCount, "acceptance must not advance the committed message count")
	var pendingMessages int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM messages WHERE turn_id = $1 AND status = 'pending'`, turn.ID).Scan(&pendingMessages))
	assert.Equal(t, 2, pendingMessages)

	leaseA, err := turns.AcquireConversationLease(ctx, owner, session.ID, turn.ID, "replica-a", time.Minute)
	require.NoError(t, err)
	assert.EqualValues(t, 1, leaseA.Epoch)
	_, _, err = turns.AcceptTurn(ctx, owner, AcceptTurnInput{
		SessionID: session.ID, ClientTurnID: "queued-turn", RequestHash: HashTurnRequest("queued"), UserContent: "queued",
	})
	require.ErrorIs(t, err, ErrTurnOutOfOrder)
	var turnRows, allMessages int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM chat_turns WHERE session_id = $1`, session.ID).Scan(&turnRows))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM messages WHERE session_id = $1`, session.ID).Scan(&allMessages))
	assert.Equal(t, 1, turnRows, "busy admission must not create a hidden queued turn")
	assert.Equal(t, 2, allMessages, "busy admission must not persist either half of the rejected turn")

	queued := insertLegacyAcceptedTurnForTest(t, db, owner, AcceptTurnInput{
		SessionID: session.ID, ClientTurnID: "legacy-queued-turn", RequestHash: HashTurnRequest("legacy-queued"), UserContent: "legacy queued",
	})
	_, err = turns.AcquireConversationLease(ctx, owner, session.ID, queued.ID, "replica-a", time.Minute)
	require.ErrorIs(t, err, ErrLeaseHeld, "a holder token for one turn cannot authorize another turn")
	_, err = turns.AcquireConversationLease(ctx, owner, session.ID, turn.ID, "replica-a", time.Minute)
	require.ErrorIs(t, err, ErrLeaseHeld, "a second handler cannot reuse a live execution lease")
	renewedLease, err := turns.RenewConversationLease(ctx, owner, leaseA, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, leaseA.Epoch, renewedLease.Epoch, "explicit renewal keeps the same epoch")
	_, err = turns.RenewConversationLease(ctx, owner, ConversationLease{
		SessionID: session.ID, TurnID: turn.ID, HolderID: "replica-a", Epoch: leaseA.Epoch - 1,
	}, time.Minute)
	require.ErrorIs(t, err, ErrLeaseFenced)
	require.NoError(t, turns.ReleaseConversationLease(ctx, owner, leaseA))
	forceTurnRetryDue(t, db, turn.ID)
	leaseB, err := turns.AcquireConversationLease(ctx, owner, session.ID, turn.ID, "replica-b", time.Minute)
	require.NoError(t, err)
	assert.Greater(t, leaseB.Epoch, leaseA.Epoch)
	running, err := turns.GetTurn(ctx, owner, turn.ID)
	require.NoError(t, err)
	assert.Equal(t, TurnStatusRunning, running.Status, "a retryable turn becomes running when a new epoch acquires it")
	require.NotNil(t, running.LeaseEpoch)
	assert.Equal(t, leaseB.Epoch, *running.LeaseEpoch)

	_, err = turns.CommitTurn(ctx, owner, CommitTurnInput{
		TurnID: turn.ID, Lease: leaseA, ExpectedContextVersion: turn.BaseContextVersion,
		ContextWriteMode: ContextWriteUpdate,
		Context:          json.RawMessage(`{"stale":true}`), Assistant: AssistantPatch{Content: "stale"},
		TerminalEventType: "turn.committed",
	})
	require.ErrorIs(t, err, ErrLeaseFenced, "an old epoch cannot commit after takeover")

	_, err = turns.AppendEvent(ctx, owner, leaseA, "stale", nil, true)
	require.ErrorIs(t, err, ErrLeaseFenced)
	_, err = turns.AppendEvent(ctx, owner, leaseB, "turn.must-not-look-terminal", nil, false)
	require.ErrorIs(t, err, ErrInvalidArgument, "only CommitTurn may append a non-provisional event")
	e1, err := turns.AppendEvent(ctx, owner, leaseB, "turn.started", json.RawMessage(`{"source":"integration"}`), true)
	require.NoError(t, err)
	e2, err := turns.AppendEvent(ctx, owner, leaseB, "turn.progress", nil, true)
	require.NoError(t, err)
	assert.Greater(t, e1.Seq, int64(0))
	assert.Equal(t, e1.Seq+1, e2.Seq)
	events, err := turns.ListEvents(ctx, owner, turn.ID, e1.Seq, 20)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, e2.Seq, events[0].Seq)
	assert.True(t, events[0].Provisional)

	interactionPayload := json.RawMessage(`{"question":"continue?","options":[1,2]}`)
	interaction, created, err := turns.CreateInteraction(ctx, owner, leaseB, "confirm-1", "confirmation", interactionPayload, time.Minute)
	require.NoError(t, err)
	require.True(t, created)
	sameInteraction, created, err := turns.CreateInteraction(ctx, owner, leaseB, "confirm-1", "confirmation", json.RawMessage(`{"options":[1,2],"question":"continue?"}`), time.Minute)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, interaction.ID, sameInteraction.ID)
	_, _, err = turns.CreateInteraction(ctx, owner, leaseB, "confirm-1", "confirmation", json.RawMessage(`{"question":"other"}`), time.Minute)
	require.ErrorIs(t, err, ErrInteractionConflict)

	secondProcess := NewPostgresTurnStore(db)
	visible, err := secondProcess.GetInteraction(ctx, owner, turn.ID, "confirm-1")
	require.NoError(t, err)
	assert.Equal(t, interaction.ID, visible.ID, "interaction must be visible to another store instance")
	response := json.RawMessage(`{"confirmed":true,"choice":1}`)
	resolved, err := secondProcess.ResolveInteraction(ctx, owner, turn.ID, "confirm-1", response)
	require.NoError(t, err)
	assert.Equal(t, InteractionStatusResolved, resolved.Status)
	_, err = turns.ResolveInteraction(ctx, owner, turn.ID, "confirm-1", json.RawMessage(`{"choice":1,"confirmed":true}`))
	require.NoError(t, err, "same response is idempotent")
	_, err = turns.ResolveInteraction(ctx, owner, turn.ID, "confirm-1", json.RawMessage(`{"confirmed":false}`))
	require.ErrorIs(t, err, ErrInteractionConflict)
	events, err = turns.ListEvents(ctx, owner, turn.ID, 0, 20)
	require.NoError(t, err)
	require.Len(t, events, 4)
	assert.Equal(t, "interaction.requested", events[2].Type)
	assert.True(t, events[2].Provisional)
	assert.Equal(t, "interaction.resolved", events[3].Type)
	assert.True(t, events[3].Provisional)

	actionHash := HashTurnRequest("StopInstance", "uhost-1")
	action, created, err := turns.ReserveAction(ctx, owner, leaseB, ReserveActionInput{Index: 0, ActionName: "StopInstance", ArgsHash: actionHash})
	require.NoError(t, err)
	require.True(t, created)
	sameAction, created, err := turns.ReserveAction(ctx, owner, leaseB, ReserveActionInput{Index: 0, ActionName: "StopInstance", ArgsHash: actionHash})
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, action.ExecutionToken, sameAction.ExecutionToken)
	semanticDuplicate, created, err := turns.ReserveAction(ctx, owner, leaseB, ReserveActionInput{Index: 1, ActionName: "StopInstance", ArgsHash: actionHash})
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, action.ExecutionToken, semanticDuplicate.ExecutionToken)
	assert.Equal(t, 0, semanticDuplicate.Index, "same mutation identity must reuse its first durable slot")
	_, _, err = turns.ReserveAction(ctx, owner, leaseB, ReserveActionInput{Index: 0, ActionName: "StopInstance", ArgsHash: HashTurnRequest("other")})
	require.ErrorIs(t, err, ErrActionConflict)
	action, err = turns.StartAction(ctx, owner, leaseB, action.ExecutionToken)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE conversation_leases SET lease_until = NOW() - INTERVAL '1 second' WHERE session_id = $1`, session.ID)
	require.NoError(t, err)
	action, err = turns.RecordAction(ctx, owner, action.ExecutionToken, ActionStatusSucceeded, json.RawMessage(`{"ok":true}`), nil)
	require.NoError(t, err, "the real side-effect result must remain recordable after lease loss")
	_, err = turns.RecordAction(ctx, owner, action.ExecutionToken, ActionStatusSucceeded, json.RawMessage(`{"ok":true}`), nil)
	require.NoError(t, err, "same terminal action record is idempotent")
	lateRequestID := "req-late-observed"
	action, err = turns.RecordAction(ctx, owner, action.ExecutionToken, ActionStatusSucceeded, json.RawMessage(`{"ok":true}`), nil, &lateRequestID)
	require.NoError(t, err, "a known upstream request id may be backfilled after the terminal result")
	require.NotNil(t, action.UpstreamRequestID)
	assert.Equal(t, lateRequestID, *action.UpstreamRequestID)
	leaseB2, err := turns.AcquireConversationLease(ctx, owner, session.ID, turn.ID, "replica-b2", time.Minute)
	require.NoError(t, err)
	assert.Greater(t, leaseB2.Epoch, leaseB.Epoch)

	commitInput := CommitTurnInput{
		TurnID:                 turn.ID,
		Lease:                  leaseB2,
		ExpectedContextVersion: turn.BaseContextVersion,
		ContextWriteMode:       ContextWriteUpdate,
		Context:                json.RawMessage(`{"schema_version":"1.0","last_intent":"chat"}`),
		Assistant:              AssistantPatch{Content: "world"},
		TerminalEventType:      "turn.committed",
		TerminalEventPayload:   json.RawMessage(`{"saved":true}`),
	}
	committed, err := turns.CommitTurn(ctx, owner, commitInput)
	require.NoError(t, err)
	assert.Equal(t, TurnStatusCommitted, committed.Status)
	require.NotNil(t, committed.CommittedContextVersion)
	assert.Equal(t, 1, *committed.CommittedContextVersion)
	_, err = turns.CommitTurn(ctx, owner, commitInput)
	require.ErrorIs(t, err, ErrLeaseFenced, "a committed lease is released and cannot be reused; callers reconcile with GetTurn")
	reconciledCommit, err := turns.GetTurn(ctx, owner, turn.ID)
	require.NoError(t, err)
	assert.Equal(t, TurnStatusCommitted, reconciledCommit.Status)
	reconciledCommit, err = turns.ReconcileCommit(ctx, owner, commitInput)
	require.NoError(t, err)
	assert.Equal(t, TurnStatusCommitted, reconciledCommit.Status)
	differentCommit := commitInput
	differentCommit.Assistant.Content = "different executor answer"
	_, err = turns.ReconcileCommit(ctx, owner, differentCommit)
	require.ErrorIs(t, err, ErrCommitConflict)

	var contextVersion int
	var contextJSON []byte
	require.NoError(t, db.QueryRow(`SELECT context_version, context, message_count FROM sessions WHERE id = $1`, session.ID).Scan(&contextVersion, &contextJSON, &messageCount))
	assert.Equal(t, 1, contextVersion)
	assert.Equal(t, 2, messageCount)
	assert.JSONEq(t, string(commitInput.Context), string(contextJSON))
	var okMessages int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM messages WHERE turn_id = $1 AND status = 'ok'`, turn.ID).Scan(&okMessages))
	assert.Equal(t, 2, okMessages)
	var answer string
	require.NoError(t, db.QueryRow(`SELECT content FROM messages WHERE id = $1`, turn.AssistantMessageID).Scan(&answer))
	assert.Equal(t, "world", answer)
	events, err = turns.ListEvents(ctx, owner, turn.ID, 0, 20)
	require.NoError(t, err)
	require.Len(t, events, 1, "takeover hides all provisional output from the expired executor")
	assert.Equal(t, "turn.committed", events[0].Type)
	assert.False(t, events[0].Provisional, "the durable commit event is terminal, not provisional")

	_, _, err = turns.AcceptTurn(ctx, owner, AcceptTurnInput{
		SessionID: session.ID, ClientTurnID: "later-queued-turn", RequestHash: HashTurnRequest("later"), UserContent: "later",
	})
	require.ErrorIs(t, err, ErrTurnOutOfOrder, "new admission remains closed while an upgraded legacy turn is pending")
	later := insertLegacyAcceptedTurnForTest(t, db, owner, AcceptTurnInput{
		SessionID: session.ID, ClientTurnID: "legacy-later-turn", RequestHash: HashTurnRequest("legacy-later"), UserContent: "legacy later",
	})
	_, err = turns.AcquireConversationLease(ctx, owner, session.ID, later.ID, "replica-out-of-order", time.Minute)
	require.ErrorIs(t, err, ErrTurnOutOfOrder, "an upgraded legacy turn cannot overtake the queue head")
	queuedLease, err := turns.AcquireConversationLease(ctx, owner, session.ID, queued.ID, "replica-queued", time.Minute)
	require.NoError(t, err)
	_, err = turns.FailTurn(ctx, owner, queuedLease, TurnStatusAborted, "test_queue_cleanup")
	require.NoError(t, err)
	laterLease, err := turns.AcquireConversationLease(ctx, owner, session.ID, later.ID, "replica-later", time.Minute)
	require.NoError(t, err)
	_, err = turns.FailTurn(ctx, owner, laterLease, TurnStatusAborted, "test_queue_cleanup")
	require.NoError(t, err)

	t.Run("pending and expired confirmation both remain hard gates", func(t *testing.T) {
		candidate, _, acceptErr := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
			SessionID: session.ID, ClientTurnID: "expired-confirmation", RequestHash: HashTurnRequest("expired-confirmation"), UserContent: "expired-confirmation",
		})
		require.NoError(t, acceptErr)
		lease, acquireErr := turns.AcquireConversationLease(ctx, owner, session.ID, candidate.ID, "replica-confirmation", time.Minute)
		require.NoError(t, acquireErr)
		interaction, _, createErr := turns.CreateInteraction(ctx, owner, lease, "expires", "confirmation", json.RawMessage(`{"question":"continue?"}`), time.Minute)
		require.NoError(t, createErr)
		_, _, reserveErr := turns.ReserveAction(ctx, owner, lease, ReserveActionInput{Index: 0, ActionName: "MustWait", ArgsHash: HashTurnRequest("wait")})
		require.ErrorIs(t, reserveErr, ErrInteractionPending)
		_, updateErr := db.Exec(`UPDATE turn_interactions SET expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, interaction.ID)
		require.NoError(t, updateErr)
		_, resolveErr := secondProcess.ResolveInteraction(ctx, owner, candidate.ID, "expires", json.RawMessage(`{"confirmed":true}`))
		require.ErrorIs(t, resolveErr, ErrInteractionExpired)
		_, _, reserveErr = turns.ReserveAction(ctx, owner, lease, ReserveActionInput{Index: 0, ActionName: "MustWait", ArgsHash: HashTurnRequest("wait")})
		require.ErrorIs(t, reserveErr, ErrInteractionExpired)
		_, commitErr := turns.CommitTurn(ctx, owner, CommitTurnInput{
			TurnID: candidate.ID, Lease: lease, ExpectedContextVersion: candidate.BaseContextVersion,
			ContextWriteMode: ContextWriteUpdate,
			Context:          json.RawMessage(`{"must":"not-commit"}`), Assistant: AssistantPatch{Content: "must not commit"},
			TerminalEventType: "turn.committed",
		})
		require.ErrorIs(t, commitErr, ErrInteractionExpired)
		_, abortErr := turns.FailTurn(ctx, owner, lease, TurnStatusAborted, "confirmation_expired")
		require.NoError(t, abortErr)
	})

	t.Run("context conflict leaves the accepted turn pending", func(t *testing.T) {
		candidate, _, acceptErr := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
			SessionID: session.ID, ClientTurnID: "context-conflict", RequestHash: HashTurnRequest("conflict"), UserContent: "conflict",
		})
		require.NoError(t, acceptErr)
		lease, acquireErr := turns.AcquireConversationLease(ctx, owner, session.ID, candidate.ID, "replica-c", time.Minute)
		require.NoError(t, acquireErr)
		_, updateErr := NewSessionStore(db).UpdateContext(ctx, owner, session.ID, json.RawMessage(`{"external":true}`), candidate.BaseContextVersion)
		require.NoError(t, updateErr)
		_, commitErr := turns.CommitTurn(ctx, owner, CommitTurnInput{
			TurnID: candidate.ID, Lease: lease, ExpectedContextVersion: candidate.BaseContextVersion,
			ContextWriteMode: ContextWriteUpdate,
			Context:          json.RawMessage(`{"loser":true}`), Assistant: AssistantPatch{Content: "must not persist"}, TerminalEventType: "turn.committed",
		})
		require.ErrorIs(t, commitErr, ErrContextConflict)
		got, getErr := turns.GetTurn(ctx, owner, candidate.ID)
		require.NoError(t, getErr)
		assert.Equal(t, TurnStatusRunning, got.Status)
		require.NoError(t, turns.ReleaseConversationLease(ctx, owner, lease))
		forceTurnRetryDue(t, db, candidate.ID)
		cleanupLease, cleanupErr := turns.AcquireConversationLease(ctx, owner, session.ID, candidate.ID, "replica-c-cleanup", time.Minute)
		require.NoError(t, cleanupErr)
		_, cleanupErr = turns.FailTurn(ctx, owner, cleanupLease, TurnStatusAborted, "test_cleanup")
		require.NoError(t, cleanupErr)
	})

	t.Run("late SQL error rolls back the whole commit", func(t *testing.T) {
		latest, getErr := NewSessionStore(db).GetByID(ctx, owner, session.ID)
		require.NoError(t, getErr)
		candidate, _, acceptErr := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
			SessionID: session.ID, ClientTurnID: "atomic-rollback", RequestHash: HashTurnRequest("rollback"), UserContent: "rollback",
		})
		require.NoError(t, acceptErr)
		lease, acquireErr := turns.AcquireConversationLease(ctx, owner, session.ID, candidate.ID, "replica-d", time.Minute)
		require.NoError(t, acquireErr)
		_, commitErr := turns.CommitTurn(ctx, owner, CommitTurnInput{
			TurnID: candidate.ID, Lease: lease, ExpectedContextVersion: latest.ContextVersion,
			ContextWriteMode: ContextWriteUpdate,
			Context:          json.RawMessage(`{"must":"rollback"}`), Assistant: AssistantPatch{Content: "must rollback"},
			TerminalEventType: strings.Repeat("x", 65),
		})
		require.Error(t, commitErr)
		after, getErr := NewSessionStore(db).GetByID(ctx, owner, session.ID)
		require.NoError(t, getErr)
		assert.Equal(t, latest.ContextVersion, after.ContextVersion)
		assert.Equal(t, latest.MessageCount, after.MessageCount)
		got, getErr := turns.GetTurn(ctx, owner, candidate.ID)
		require.NoError(t, getErr)
		assert.Equal(t, TurnStatusRunning, got.Status)
		var statuses string
		require.NoError(t, db.QueryRow(`SELECT string_agg(status, ',' ORDER BY turn_role) FROM messages WHERE turn_id = $1`, candidate.ID).Scan(&statuses))
		assert.Equal(t, "pending,pending", statuses)
		require.NoError(t, turns.ReleaseConversationLease(ctx, owner, lease))
		forceTurnRetryDue(t, db, candidate.ID)
		cleanupLease, cleanupErr := turns.AcquireConversationLease(ctx, owner, session.ID, candidate.ID, "replica-d-cleanup", time.Minute)
		require.NoError(t, cleanupErr)
		_, cleanupErr = turns.FailTurn(ctx, owner, cleanupLease, TurnStatusAborted, "test_cleanup")
		require.NoError(t, cleanupErr)
	})

	t.Run("a non-pending message rejects commit without partial writes", func(t *testing.T) {
		latest, getErr := NewSessionStore(db).GetByID(ctx, owner, session.ID)
		require.NoError(t, getErr)
		candidate, _, acceptErr := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
			SessionID: session.ID, ClientTurnID: "message-state-conflict", RequestHash: HashTurnRequest("message-state"), UserContent: "message-state",
		})
		require.NoError(t, acceptErr)
		lease, acquireErr := turns.AcquireConversationLease(ctx, owner, session.ID, candidate.ID, "replica-message", time.Minute)
		require.NoError(t, acquireErr)
		_, updateErr := db.Exec(`UPDATE messages SET status = 'error' WHERE id = $1`, candidate.UserMessageID)
		require.NoError(t, updateErr)
		_, commitErr := turns.CommitTurn(ctx, owner, CommitTurnInput{
			TurnID: candidate.ID, Lease: lease, ExpectedContextVersion: latest.ContextVersion,
			ContextWriteMode: ContextWriteUpdate,
			Context:          json.RawMessage(`{"must":"not-write"}`), Assistant: AssistantPatch{Content: "must not persist"},
			TerminalEventType: "turn.committed",
		})
		require.ErrorIs(t, commitErr, ErrInvalidTurnState)
		after, getErr := NewSessionStore(db).GetByID(ctx, owner, session.ID)
		require.NoError(t, getErr)
		assert.Equal(t, latest.ContextVersion, after.ContextVersion)
		assert.Equal(t, latest.MessageCount, after.MessageCount)
		require.NoError(t, turns.ReleaseConversationLease(ctx, owner, lease))
		forceTurnRetryDue(t, db, candidate.ID)
		cleanupLease, cleanupErr := turns.AcquireConversationLease(ctx, owner, session.ID, candidate.ID, "replica-message-cleanup", time.Minute)
		require.NoError(t, cleanupErr)
		_, cleanupErr = turns.FailTurn(ctx, owner, cleanupLease, TurnStatusAborted, "test_cleanup")
		require.NoError(t, cleanupErr)
	})

	t.Run("reconcile marks a turn ambiguous when an action may have executed", func(t *testing.T) {
		candidate, _, acceptErr := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
			SessionID: session.ID, ClientTurnID: "ambiguous-action", RequestHash: HashTurnRequest("action"), UserContent: "action",
		})
		require.NoError(t, acceptErr)
		lease, acquireErr := turns.AcquireConversationLease(ctx, owner, session.ID, candidate.ID, "replica-e", time.Minute)
		require.NoError(t, acquireErr)
		action, _, reserveErr := turns.ReserveAction(ctx, owner, lease, ReserveActionInput{Index: 0, ActionName: "ExternalWrite", ArgsHash: HashTurnRequest("external-write")})
		require.NoError(t, reserveErr)
		_, reserveErr = turns.StartAction(ctx, owner, lease, action.ExecutionToken)
		require.NoError(t, reserveErr)
		reconciled, reconcileErr := turns.ReconcileTurn(ctx, owner, lease, "worker_lost")
		require.NoError(t, reconcileErr)
		assert.Equal(t, TurnStatusAmbiguousAfterAction, reconciled.Status)
		again, getErr := turns.GetTurn(ctx, owner, candidate.ID)
		require.NoError(t, getErr)
		assert.Equal(t, TurnStatusAmbiguousAfterAction, again.Status)
	})

	t.Run("ordinary release cannot make an uncertain action retryable", func(t *testing.T) {
		candidate, _, acceptErr := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
			SessionID: session.ID, ClientTurnID: "release-ambiguous-action", RequestHash: HashTurnRequest("release-action"), UserContent: "release-action",
		})
		require.NoError(t, acceptErr)
		lease, acquireErr := turns.AcquireConversationLease(ctx, owner, session.ID, candidate.ID, "replica-release", time.Minute)
		require.NoError(t, acquireErr)
		action, _, reserveErr := turns.ReserveAction(ctx, owner, lease, ReserveActionInput{Index: 0, ActionName: "ExternalWrite", ArgsHash: HashTurnRequest("release-write")})
		require.NoError(t, reserveErr)
		action, reserveErr = turns.StartAction(ctx, owner, lease, action.ExecutionToken)
		require.NoError(t, reserveErr)
		require.NoError(t, turns.ReleaseConversationLease(ctx, owner, lease))
		released, getErr := turns.GetTurn(ctx, owner, candidate.ID)
		require.NoError(t, getErr)
		assert.Equal(t, TurnStatusAmbiguousAfterAction, released.Status)
		_, recordErr := turns.RecordAction(ctx, owner, action.ExecutionToken, ActionStatusSucceeded, json.RawMessage(`{"ok":true}`), nil)
		require.NoError(t, recordErr, "the true result remains recordable after ambiguous release")
	})

	t.Run("fail without an action stays retryable", func(t *testing.T) {
		candidate, _, acceptErr := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
			SessionID: session.ID, ClientTurnID: "safe-failure", RequestHash: HashTurnRequest("safe"), UserContent: "safe",
		})
		require.NoError(t, acceptErr)
		lease, acquireErr := turns.AcquireConversationLease(ctx, owner, session.ID, candidate.ID, "replica-f", time.Minute)
		require.NoError(t, acquireErr)
		failed, failErr := turns.FailTurn(ctx, owner, lease, TurnStatusFailedRetryable, "model_unavailable")
		require.NoError(t, failErr)
		assert.Equal(t, TurnStatusFailedRetryable, failed.Status)
		forceTurnRetryDue(t, db, candidate.ID)
		cleanupLease, cleanupErr := turns.AcquireConversationLease(ctx, owner, session.ID, candidate.ID, "replica-f-cleanup", time.Minute)
		require.NoError(t, cleanupErr)
		_, cleanupErr = turns.FailTurn(ctx, owner, cleanupLease, TurnStatusAborted, "test_cleanup")
		require.NoError(t, cleanupErr)
	})

	t.Run("a definitely failed action remains retryable", func(t *testing.T) {
		candidate, _, acceptErr := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
			SessionID: session.ID, ClientTurnID: "failed-action", RequestHash: HashTurnRequest("failed-action"), UserContent: "failed-action",
		})
		require.NoError(t, acceptErr)
		lease, acquireErr := turns.AcquireConversationLease(ctx, owner, session.ID, candidate.ID, "replica-g", time.Minute)
		require.NoError(t, acquireErr)
		action, _, reserveErr := turns.ReserveAction(ctx, owner, lease, ReserveActionInput{Index: 0, ActionName: "ExternalWrite", ArgsHash: HashTurnRequest("failed-write")})
		require.NoError(t, reserveErr)
		action, reserveErr = turns.StartAction(ctx, owner, lease, action.ExecutionToken)
		require.NoError(t, reserveErr)
		code := "upstream_rejected"
		_, recordErr := turns.RecordAction(ctx, owner, action.ExecutionToken, ActionStatusFailed, json.RawMessage(`{"accepted":false}`), &code)
		require.NoError(t, recordErr)
		reconciled, reconcileErr := turns.ReconcileTurn(ctx, owner, lease, "worker_lost")
		require.NoError(t, reconcileErr)
		assert.Equal(t, TurnStatusFailedRetryable, reconciled.Status)
		forceTurnRetryDue(t, db, candidate.ID)
		cleanupLease, cleanupErr := turns.AcquireConversationLease(ctx, owner, session.ID, candidate.ID, "replica-g-cleanup", time.Minute)
		require.NoError(t, cleanupErr)
		_, cleanupErr = turns.FailTurn(ctx, owner, cleanupLease, TurnStatusAborted, "test_cleanup")
		require.NoError(t, cleanupErr)
	})

	t.Run("the lease row cannot authorize a differently bound turn executor", func(t *testing.T) {
		candidate, _, acceptErr := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
			SessionID: session.ID, ClientTurnID: "binding-fence", RequestHash: HashTurnRequest("binding-fence"), UserContent: "binding-fence",
		})
		require.NoError(t, acceptErr)
		lease, acquireErr := turns.AcquireConversationLease(ctx, owner, session.ID, candidate.ID, "replica-binding", time.Minute)
		require.NoError(t, acquireErr)
		_, updateErr := db.Exec(`UPDATE chat_turns SET executor_id = 'different-executor' WHERE id = $1`, candidate.ID)
		require.NoError(t, updateErr)
		_, renewErr := turns.RenewConversationLease(ctx, owner, lease, time.Minute)
		require.ErrorIs(t, renewErr, ErrLeaseFenced)
		_, appendErr := turns.AppendEvent(ctx, owner, lease, "must.not.append", nil, true)
		require.ErrorIs(t, appendErr, ErrLeaseFenced)
		_, commitErr := turns.CommitTurn(ctx, owner, CommitTurnInput{
			TurnID: candidate.ID, Lease: lease, ExpectedContextVersion: candidate.BaseContextVersion,
			ContextWriteMode: ContextWriteUpdate,
			Context:          json.RawMessage(`{"must":"not-commit"}`), Assistant: AssistantPatch{Content: "must not commit"},
			TerminalEventType: "turn.committed",
		})
		require.ErrorIs(t, commitErr, ErrLeaseFenced)
	})

	_, err = turns.AppendEvent(ctx, owner, leaseA, "old-epoch", nil, true)
	assert.True(t, errors.Is(err, ErrLeaseFenced), fmt.Sprintf("old epoch must remain fenced, got %v", err))
}

func TestPostgresTurnStore_ActionReservationStartsOnlyUnderCurrentLease(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 72001, OrganizationID: 72002}
	session, err := NewSessionStore(db).Create(ctx, owner, nil, json.RawMessage(`{"schema_version":"1.0"}`))
	require.NoError(t, err)
	turns := NewPostgresTurnStore(db)
	turn, _, err := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
		SessionID: session.ID, ClientTurnID: "action-two-phase", RequestHash: HashTurnRequest("action-two-phase"), UserContent: "stop it",
	})
	require.NoError(t, err)
	leaseA, err := turns.AcquireConversationLease(ctx, owner, session.ID, turn.ID, "replica-a", time.Minute)
	require.NoError(t, err)

	reserved, created, err := turns.ReserveAction(ctx, owner, leaseA, ReserveActionInput{
		Index: 0, ActionName: "StopCompShareInstance", ArgsHash: HashTurnRequest("canonical-args"),
	})
	require.NoError(t, err)
	require.True(t, created)
	assert.False(t, reserved.InFlight, "reservation must be durable before upstream execution starts")
	_, err = turns.CommitTurn(ctx, owner, CommitTurnInput{
		TurnID: turn.ID, Lease: leaseA, ExpectedContextVersion: turn.BaseContextVersion,
		ContextWriteMode: ContextWriteUpdate, Context: json.RawMessage(`{"schema_version":"1.0"}`),
		Assistant: AssistantPatch{Content: "must wait for action"}, TerminalEventType: "turn.committed",
	})
	require.ErrorIs(t, err, ErrActionUncertain, "even an unstarted reservation must be terminal before answer commit")

	// Simulate a crash after reservation but before StartAction. The next lease
	// may safely claim the same durable slot exactly once.
	_, err = db.Exec(`UPDATE conversation_leases SET lease_until = NOW() - INTERVAL '1 second' WHERE session_id = $1`, session.ID)
	require.NoError(t, err)
	leaseB, err := turns.AcquireConversationLease(ctx, owner, session.ID, turn.ID, "replica-b", time.Minute)
	require.NoError(t, err)
	same, created, err := turns.ReserveAction(ctx, owner, leaseB, ReserveActionInput{
		Index: 0, ActionName: "StopCompShareInstance", ArgsHash: HashTurnRequest("canonical-args"),
	})
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, reserved.ExecutionToken, same.ExecutionToken)

	started, err := turns.StartAction(ctx, owner, leaseB, same.ExecutionToken)
	require.NoError(t, err)
	assert.True(t, started.InFlight)
	assert.Equal(t, leaseB.Epoch, started.LeaseEpoch)
	_, err = turns.StartAction(ctx, owner, leaseB, same.ExecutionToken)
	require.ErrorIs(t, err, ErrActionUncertain, "a started call must never be issued twice")
}

func TestPostgresTurnStore_RejectsEmptySemanticMessages(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 72201, OrganizationID: 72202}
	session, err := NewSessionStore(db).Create(ctx, owner, nil, nil)
	require.NoError(t, err)
	turns := NewPostgresTurnStore(db)

	_, _, err = turns.AcceptTurn(ctx, owner, AcceptTurnInput{
		SessionID: session.ID, ClientTurnID: "blank-user", RequestHash: HashTurnRequest("blank-user"), UserContent: " \n\t ",
	})
	require.ErrorIs(t, err, ErrInvalidArgument)
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM chat_turns WHERE session_id = $1`, session.ID).Scan(&count))
	assert.Zero(t, count)

	turn, _, err := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
		SessionID: session.ID, ClientTurnID: "blank-assistant", RequestHash: HashTurnRequest("blank-assistant"), UserContent: "valid",
	})
	require.NoError(t, err)
	lease, err := turns.AcquireConversationLease(ctx, owner, session.ID, turn.ID, "replica", time.Minute)
	require.NoError(t, err)
	_, err = turns.CommitTurn(ctx, owner, CommitTurnInput{
		TurnID: turn.ID, Lease: lease, ExpectedContextVersion: turn.BaseContextVersion,
		ContextWriteMode: ContextWriteUpdate, Context: json.RawMessage(`{"schema_version":"1.0"}`),
		Assistant: AssistantPatch{Content: " \n\t "}, TerminalEventType: "turn.committed",
	})
	require.ErrorIs(t, err, ErrInvalidArgument)
	uncommitted, err := turns.GetTurn(ctx, owner, turn.ID)
	require.NoError(t, err)
	assert.NotEqual(t, TurnStatusCommitted, uncommitted.Status)
}

func TestPostgresTurnStore_ReconcileDistinguishesSafeReservationFromUnknownOutcome(t *testing.T) {
	tests := []struct {
		name       string
		start      bool
		finish     ActionStatus
		wantStatus TurnStatus
	}{
		{name: "reserved but never started is retryable", wantStatus: TurnStatusFailedRetryable},
		{name: "started result unknown is ambiguous", start: true, wantStatus: TurnStatusAmbiguousAfterAction},
		{name: "succeeded result is known and replayable", start: true, finish: ActionStatusSucceeded, wantStatus: TurnStatusFailedRetryable},
		{name: "definite upstream failure is known", start: true, finish: ActionStatusFailed, wantStatus: TurnStatusFailedRetryable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := openIsolatedTurnTestDB(t)
			ctx := context.Background()
			owner := Owner{TopOrganizationID: 72101, OrganizationID: 72102}
			session, err := NewSessionStore(db).Create(ctx, owner, nil, json.RawMessage(`{"schema_version":"1.0"}`))
			require.NoError(t, err)
			turns := NewPostgresTurnStore(db)
			turn, _, err := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
				SessionID: session.ID, ClientTurnID: "reconcile-action", RequestHash: HashTurnRequest("reconcile-action"), UserContent: "change it",
			})
			require.NoError(t, err)
			lease, err := turns.AcquireConversationLease(ctx, owner, session.ID, turn.ID, "replica-a", time.Minute)
			require.NoError(t, err)
			action, _, err := turns.ReserveAction(ctx, owner, lease, ReserveActionInput{
				Index: 0, ActionName: "StopCompShareInstance", ArgsHash: HashTurnRequest("args"),
			})
			require.NoError(t, err)
			if tc.start {
				action, err = turns.StartAction(ctx, owner, lease, action.ExecutionToken)
				require.NoError(t, err)
			}
			if tc.finish != "" {
				var code *string
				result := json.RawMessage(`{"RetCode":0,"RequestId":"req-known"}`)
				if tc.finish == ActionStatusFailed {
					v := "upstream_api:230"
					code = &v
					result = json.RawMessage(`{"code":230,"message":"capacity"}`)
				}
				requestID := "req-known"
				action, err = turns.RecordAction(ctx, owner, action.ExecutionToken, tc.finish, result, code, &requestID)
				require.NoError(t, err)
				assert.Equal(t, requestID, *action.UpstreamRequestID)
			}
			reconciled, err := turns.ReconcileTurn(ctx, owner, lease, "worker_lost")
			require.NoError(t, err)
			assert.Equal(t, tc.wantStatus, reconciled.Status)
		})
	}
}

func TestPostgresTurnStore_ConcurrentFencing(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 72001, OrganizationID: 72002}
	session, err := NewSessionStore(db).Create(ctx, owner, nil, nil)
	require.NoError(t, err)

	stores := []*PostgresTurnStore{NewPostgresTurnStore(db), NewPostgresTurnStore(db)}
	type acceptResult struct {
		turn    Turn
		created bool
		err     error
	}
	acceptResults := make(chan acceptResult, len(stores))
	start := make(chan struct{})
	for _, turnStore := range stores {
		go func(s *PostgresTurnStore) {
			<-start
			turn, created, acceptErr := s.AcceptTurn(ctx, owner, AcceptTurnInput{
				SessionID: session.ID, ClientTurnID: "same-client-turn",
				RequestHash: HashTurnRequest("same-request"), UserContent: "same request",
			})
			acceptResults <- acceptResult{turn: turn, created: created, err: acceptErr}
		}(turnStore)
	}
	close(start)
	first := <-acceptResults
	second := <-acceptResults
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	assert.Equal(t, first.turn.ID, second.turn.ID)
	assert.NotEqual(t, first.created, second.created, "exactly one concurrent accept creates the durable turn")
	var messageCount, pendingCount, turnCount, allMessageCount int
	require.NoError(t, db.QueryRow(`SELECT message_count FROM sessions WHERE id = $1`, session.ID).Scan(&messageCount))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM messages WHERE turn_id = $1 AND status = 'pending'`, first.turn.ID).Scan(&pendingCount))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM chat_turns WHERE session_id = $1`, session.ID).Scan(&turnCount))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM messages WHERE session_id = $1`, session.ID).Scan(&allMessageCount))
	assert.Zero(t, messageCount)
	assert.Equal(t, 2, pendingCount)
	assert.Equal(t, 1, turnCount)
	assert.Equal(t, 2, allMessageCount)

	type leaseResult struct {
		lease ConversationLease
		err   error
	}
	leaseResults := make(chan leaseResult, len(stores))
	start = make(chan struct{})
	for i, turnStore := range stores {
		holder := fmt.Sprintf("replica-%d", i+1)
		go func(s *PostgresTurnStore, holderID string) {
			<-start
			lease, acquireErr := s.AcquireConversationLease(ctx, owner, session.ID, first.turn.ID, holderID, time.Minute)
			leaseResults <- leaseResult{lease: lease, err: acquireErr}
		}(turnStore, holder)
	}
	close(start)
	one := <-leaseResults
	two := <-leaseResults
	var winner ConversationLease
	if one.err == nil {
		winner = one.lease
		require.ErrorIs(t, two.err, ErrLeaseHeld)
	} else {
		require.ErrorIs(t, one.err, ErrLeaseHeld)
		require.NoError(t, two.err)
		winner = two.lease
	}
	assert.EqualValues(t, 1, winner.Epoch)
	_, err = stores[0].AppendEvent(ctx, owner, winner, "old-attempt.partial", json.RawMessage(`{"text":"ghost"}`), true)
	require.NoError(t, err)

	_, err = db.Exec(`UPDATE conversation_leases SET lease_until = NOW() - INTERVAL '1 second' WHERE session_id = $1`, session.ID)
	require.NoError(t, err)
	takeover, err := stores[1].AcquireConversationLease(ctx, owner, session.ID, first.turn.ID, "replica-takeover", time.Minute)
	require.NoError(t, err)
	assert.Greater(t, takeover.Epoch, winner.Epoch)
	_, err = stores[0].AppendEvent(ctx, owner, winner, "stale-after-takeover", nil, true)
	require.ErrorIs(t, err, ErrLeaseFenced)
	currentEvent, err := stores[1].AppendEvent(ctx, owner, takeover, "new-attempt.partial", json.RawMessage(`{"text":"current"}`), true)
	require.NoError(t, err)
	replayed, err := stores[1].ListEvents(ctx, owner, first.turn.ID, 0, 10)
	require.NoError(t, err)
	require.Len(t, replayed, 1, "a takeover must not replay the old executor's provisional output")
	assert.Equal(t, currentEvent.Seq, replayed[0].Seq)
	assert.Equal(t, takeover.Epoch, replayed[0].LeaseEpoch)
	require.NoError(t, stores[1].ReleaseConversationLease(ctx, owner, takeover))
	require.NoError(t, VerifySchema(ctx, db), "schema probes must also work after protocol tables contain rows")
}

func TestPostgresTurnStore_ConcurrentDifferentClientIDsAdmitExactlyOne(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 72101, OrganizationID: 72102}
	session, err := NewSessionStore(db).Create(ctx, owner, nil, nil)
	require.NoError(t, err)

	type acceptResult struct {
		turn    Turn
		created bool
		err     error
	}
	results := make(chan acceptResult, 2)
	start := make(chan struct{})
	stores := []*PostgresTurnStore{NewPostgresTurnStore(db), NewPostgresTurnStore(db)}
	for i, turnStore := range stores {
		clientTurnID := fmt.Sprintf("different-client-%d", i+1)
		go func(s *PostgresTurnStore, id string) {
			<-start
			turn, created, acceptErr := s.AcceptTurn(ctx, owner, AcceptTurnInput{
				SessionID: session.ID, ClientTurnID: id,
				RequestHash: HashTurnRequest(id), UserContent: id,
			})
			results <- acceptResult{turn: turn, created: created, err: acceptErr}
		}(turnStore, clientTurnID)
	}
	close(start)

	successes, busy := 0, 0
	var admitted Turn
	for i := 0; i < 2; i++ {
		result := <-results
		switch {
		case result.err == nil:
			successes++
			require.True(t, result.created)
			assert.Equal(t, TurnStatusAccepted, result.turn.Status)
			admitted = result.turn
		case errors.Is(result.err, ErrTurnOutOfOrder):
			busy++
			assert.False(t, result.created)
			assert.Empty(t, result.turn.ID)
		default:
			t.Fatalf("unexpected admission result: %v", result.err)
		}
	}
	assert.Equal(t, 1, successes)
	assert.Equal(t, 1, busy)
	require.NotEmpty(t, admitted.ID)

	var turnCount, messageCount int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM chat_turns WHERE session_id = $1`, session.ID).Scan(&turnCount))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM messages WHERE session_id = $1`, session.ID).Scan(&messageCount))
	assert.Equal(t, 1, turnCount)
	assert.Equal(t, 2, messageCount)
}

func TestPostgresTurnStore_FirstDurableSequenceContinuesLegacyCommittedCount(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 72201, OrganizationID: 72202}
	session, err := NewSessionStore(db).Create(ctx, owner, nil, nil)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE sessions SET message_count = 20 WHERE id = $1`, session.ID)
	require.NoError(t, err)
	turn, _, err := NewPostgresTurnStore(db).AcceptTurn(ctx, owner, AcceptTurnInput{
		SessionID: session.ID, ClientTurnID: "first-v2-after-legacy",
		RequestHash: HashTurnRequest("first-v2-after-legacy"), UserContent: "continue",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(11), turn.Sequence)
}

func TestPostgresTurnStore_PreserveContextCommitLeavesUnknownStateUntouched(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 73001, OrganizationID: 73002}
	unknownContext := json.RawMessage(`{"schema_version":"99.0","opaque":{"new_field":true,"nested":[3,2,1]}}`)
	session, err := NewSessionStore(db).Create(ctx, owner, nil, unknownContext)
	require.NoError(t, err)

	var beforeText string
	var beforeVersion, beforeCount int
	require.NoError(t, db.QueryRow(`SELECT context::text, context_version, message_count FROM sessions WHERE id = $1`, session.ID).
		Scan(&beforeText, &beforeVersion, &beforeCount))

	turns := NewPostgresTurnStore(db)
	turn, _, err := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
		SessionID: session.ID, ClientTurnID: "preserve-unknown", RequestHash: HashTurnRequest("preserve-unknown"), UserContent: "continue",
	})
	require.NoError(t, err)
	lease, err := turns.AcquireConversationLease(ctx, owner, session.ID, turn.ID, "replica-preserve", time.Minute)
	require.NoError(t, err)
	turn, err = turns.GetTurn(ctx, owner, turn.ID)
	require.NoError(t, err)

	committed, err := turns.CommitTurn(ctx, owner, CommitTurnInput{
		TurnID: turn.ID, Lease: lease, ExpectedContextVersion: turn.BaseContextVersion,
		ContextWriteMode:  ContextWritePreserve,
		Context:           json.RawMessage(`{"schema_version":"1.0","must_not":"overwrite"}`),
		Assistant:         AssistantPatch{Content: "safe read-only answer"},
		TerminalEventType: "turn.committed",
	})
	require.NoError(t, err)
	require.NotNil(t, committed.CommittedContextVersion)
	assert.Equal(t, beforeVersion, *committed.CommittedContextVersion)

	var afterText string
	var afterVersion, afterCount int
	require.NoError(t, db.QueryRow(`SELECT context::text, context_version, message_count FROM sessions WHERE id = $1`, session.ID).
		Scan(&afterText, &afterVersion, &afterCount))
	assert.Equal(t, beforeText, afterText, "preserve mode must not rewrite even semantically equivalent JSONB state")
	assert.Equal(t, beforeVersion, afterVersion, "preserve mode must not advance the context version")
	assert.Equal(t, beforeCount+2, afterCount, "the committed user and assistant messages still count")
}

func TestPostgresTurnStore_TakeoverRebindsPendingInteractionAndReplaysItsCard(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 74001, OrganizationID: 74002}
	session, err := NewSessionStore(db).Create(ctx, owner, nil, nil)
	require.NoError(t, err)
	turns := NewPostgresTurnStore(db)
	turn, _, err := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
		SessionID: session.ID, ClientTurnID: "takeover-interaction", RequestHash: HashTurnRequest("takeover-interaction"), UserContent: "delete it",
	})
	require.NoError(t, err)
	firstLease, err := turns.AcquireConversationLease(ctx, owner, session.ID, turn.ID, "replica-first", time.Minute)
	require.NoError(t, err)
	payload := json.RawMessage(`{"question":"continue?","options":["yes","no"]}`)
	original, created, err := turns.CreateInteraction(ctx, owner, firstLease, "confirm-delete", "confirmation", payload, 10*time.Minute)
	require.NoError(t, err)
	require.True(t, created)

	resolvedOriginal, created, err := turns.CreateInteraction(ctx, owner, firstLease, "already-resolved", "confirmation", json.RawMessage(`{"question":"resolved?"}`), 10*time.Minute)
	require.NoError(t, err)
	require.True(t, created)
	resolvedOriginal, err = turns.ResolveInteraction(ctx, owner, turn.ID, "already-resolved", json.RawMessage(`{"confirmed":true}`))
	require.NoError(t, err)

	_, err = db.Exec(`UPDATE conversation_leases SET lease_until = NOW() - INTERVAL '1 second' WHERE session_id = $1`, session.ID)
	require.NoError(t, err)
	takeover, err := turns.AcquireConversationLease(ctx, owner, session.ID, turn.ID, "replica-takeover", time.Minute)
	require.NoError(t, err)
	require.Greater(t, takeover.Epoch, firstLease.Epoch)

	rebound, created, err := turns.CreateInteraction(ctx, owner, takeover, "confirm-delete", "confirmation", json.RawMessage(`{"options":["yes","no"],"question":"continue?"}`), 10*time.Minute)
	require.NoError(t, err)
	assert.False(t, created, "takeover resumes the same durable interaction instead of creating another")
	assert.Equal(t, original.ID, rebound.ID)
	assert.Equal(t, takeover.Epoch, rebound.LeaseEpoch, "the pending card belongs to the current fenced executor")

	_, created, err = turns.CreateInteraction(ctx, owner, takeover, "confirm-delete", "confirmation", payload, 10*time.Minute)
	require.NoError(t, err)
	assert.False(t, created)
	_, _, err = turns.CreateInteraction(ctx, owner, takeover, "confirm-delete", "confirmation", json.RawMessage(`{"question":"different"}`), 10*time.Minute)
	require.ErrorIs(t, err, ErrInteractionConflict)

	resolvedAfterTakeover, created, err := turns.CreateInteraction(ctx, owner, takeover, "already-resolved", "confirmation", json.RawMessage(`{"question":"resolved?"}`), 10*time.Minute)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, InteractionStatusResolved, resolvedAfterTakeover.Status)
	assert.Equal(t, resolvedOriginal.LeaseEpoch, resolvedAfterTakeover.LeaseEpoch, "resolved history must not be rebound to a new attempt")

	events, err := turns.ListEvents(ctx, owner, turn.ID, 0, 20)
	require.NoError(t, err)
	var currentRequested []TurnEvent
	for _, event := range events {
		if event.Type == "interaction.requested" {
			currentRequested = append(currentRequested, event)
		}
	}
	require.Len(t, currentRequested, 1, "resume must expose exactly one current-epoch confirmation card")
	assert.Equal(t, takeover.Epoch, currentRequested[0].LeaseEpoch)
}

func TestPostgresTurnStore_ReboundPendingInteractionRestoresAwaitingState(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 74101, OrganizationID: 74102}
	session, err := NewSessionStore(db).Create(ctx, owner, nil, nil)
	require.NoError(t, err)
	turns := NewPostgresTurnStore(db)
	turn, _, err := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
		SessionID: session.ID, ClientTurnID: "rebind-state", RequestHash: HashTurnRequest("rebind-state"), UserContent: "confirm it",
	})
	require.NoError(t, err)
	first, err := turns.AcquireConversationLease(ctx, owner, session.ID, turn.ID, "replica-first", time.Minute)
	require.NoError(t, err)
	payload := json.RawMessage(`{"question":"continue?"}`)
	_, _, err = turns.CreateInteraction(ctx, owner, first, "confirm", "confirmation", payload, 10*time.Minute)
	require.NoError(t, err)
	require.NoError(t, turns.ReleaseConversationLease(ctx, owner, first))
	forceTurnRetryDue(t, db, turn.ID)

	takeover, err := turns.AcquireConversationLease(ctx, owner, session.ID, turn.ID, "replica-next", time.Minute)
	require.NoError(t, err)
	running, err := turns.GetTurn(ctx, owner, turn.ID)
	require.NoError(t, err)
	require.Equal(t, TurnStatusRunning, running.Status, "lease acquisition starts a retryable turn")
	_, created, err := turns.CreateInteraction(ctx, owner, takeover, "confirm", "confirmation", payload, 10*time.Minute)
	require.NoError(t, err)
	assert.False(t, created)
	rebound, err := turns.GetTurn(ctx, owner, turn.ID)
	require.NoError(t, err)
	assert.Equal(t, TurnStatusAwaitingConfirmation, rebound.Status, "a pending interaction and running turn may never disagree")
}

func forceTurnRetryDue(t *testing.T, db *sql.DB, turnID string) {
	t.Helper()
	_, err := db.Exec(`UPDATE chat_turns SET next_retry_at = NOW() - INTERVAL '1 second' WHERE id = $1`, turnID)
	require.NoError(t, err)
}

func TestPostgresTurnStore_AcquireLegacyQueuedTurnRefreshesItsContextSnapshot(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 75001, OrganizationID: 75002}
	session, err := NewSessionStore(db).Create(ctx, owner, nil, json.RawMessage(`{"step":0}`))
	require.NoError(t, err)
	turns := NewPostgresTurnStore(db)
	first, _, err := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
		SessionID: session.ID, ClientTurnID: "first", RequestHash: HashTurnRequest("first"), UserContent: "first",
	})
	require.NoError(t, err)
	queued := insertLegacyAcceptedTurnForTest(t, db, owner, AcceptTurnInput{
		SessionID: session.ID, ClientTurnID: "legacy-queued", RequestHash: HashTurnRequest("legacy-queued"), UserContent: "legacy queued",
	})
	assert.Equal(t, first.BaseContextVersion, queued.BaseContextVersion, "an upgraded queued row retains its old acceptance snapshot")

	firstLease, err := turns.AcquireConversationLease(ctx, owner, session.ID, first.ID, "replica-first", time.Minute)
	require.NoError(t, err)
	first, err = turns.GetTurn(ctx, owner, first.ID)
	require.NoError(t, err)
	_, err = turns.CommitTurn(ctx, owner, CommitTurnInput{
		TurnID: first.ID, Lease: firstLease, ExpectedContextVersion: first.BaseContextVersion,
		ContextWriteMode: ContextWriteUpdate, Context: json.RawMessage(`{"step":1}`),
		Assistant: AssistantPatch{Content: "first answer"}, TerminalEventType: "turn.committed",
	})
	require.NoError(t, err)

	queuedLease, err := turns.AcquireConversationLease(ctx, owner, session.ID, queued.ID, "replica-queued", time.Minute)
	require.NoError(t, err)
	queued, err = turns.GetTurn(ctx, owner, queued.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, queued.BaseContextVersion, "upgrade recovery must bind the latest committed snapshot, not the legacy acceptance snapshot")

	_, err = turns.CommitTurn(ctx, owner, CommitTurnInput{
		TurnID: queued.ID, Lease: queuedLease, ExpectedContextVersion: queued.BaseContextVersion,
		ContextWriteMode: ContextWriteUpdate, Context: json.RawMessage(`{"step":2}`),
		Assistant: AssistantPatch{Content: "queued answer"}, TerminalEventType: "turn.committed",
	})
	require.NoError(t, err)
}
