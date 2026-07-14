package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgresTurnStore_DurableRetryBudgetSurvivesRestartAndReleasesQueue(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 81001, OrganizationID: 81002}
	sessions := NewSessionStore(db)
	session, err := sessions.Create(ctx, owner, nil, json.RawMessage(`{"schema_version":"1.0"}`))
	require.NoError(t, err)
	turns := NewPostgresTurnStore(db)
	first, _, err := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
		SessionID: session.ID, ClientTurnID: "durable-retry", RequestHash: HashTurnRequest("durable-retry"),
		UserContent: "retry me", ExecutionEnvelope: json.RawMessage(`{"version":1,"message":"retry me"}`),
	})
	require.NoError(t, err)
	queued, _, err := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
		SessionID: session.ID, ClientTurnID: "after-final", RequestHash: HashTurnRequest("after-final"),
		UserContent: "next", ExecutionEnvelope: json.RawMessage(`{"version":1,"message":"next"}`),
	})
	require.NoError(t, err)

	lease, err := turns.AcquireConversationLease(ctx, owner, session.ID, first.ID, "initial", time.Minute)
	require.NoError(t, err)
	actionHint := json.RawMessage(`{"resource_ids":["uhost-known"]}`)
	action, _, err := turns.ReserveAction(ctx, owner, lease, ReserveActionInput{
		Index: 0, ActionName: "KnownSuccessfulWrite", ArgsHash: HashTurnRequest("known-write"), ContextHint: actionHint,
	})
	require.NoError(t, err)
	action, err = turns.StartAction(ctx, owner, lease, action.ExecutionToken)
	require.NoError(t, err)
	_, err = turns.RecordActionWithContext(ctx, owner, action.ExecutionToken, ActionStatusSucceeded,
		json.RawMessage(`{"RetCode":0}`), nil, nil, actionHint)
	require.NoError(t, err)
	retrying, err := turns.FailTurn(ctx, owner, lease, TurnStatusFailedRetryable, "transient")
	require.NoError(t, err)
	require.Equal(t, 1, retrying.RetryCount)
	require.NotNil(t, retrying.NextRetryAt)
	assert.WithinDuration(t, time.Now().Add(2*time.Second), *retrying.NextRetryAt, 2*time.Second)
	assert.NotEmpty(t, retrying.ExecutionEnvelope)

	recoverable, err := turns.ListRecoverableTurns(ctx, 10)
	require.NoError(t, err)
	for _, candidate := range recoverable {
		assert.NotEqual(t, first.ID, candidate.Turn.ID, "a retryable row must stay out of the scan until its durable deadline")
	}
	_, err = turns.AcquireConversationLease(ctx, owner, session.ID, first.ID, "too-early", time.Minute)
	require.ErrorIs(t, err, ErrRetryNotDue)

	// A new store instance observes the same attempt count: process restart does
	// not reset the budget.
	restarted := NewPostgresTurnStore(db)
	afterRestart, err := restarted.GetTurn(ctx, owner, first.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, afterRestart.RetryCount)

	for expectedCount := 2; expectedCount <= MaxTurnRecoveryAttempts; expectedCount++ {
		forceTurnRetryDue(t, db, first.ID)
		if expectedCount == 2 {
			// Two replicas may scan the same due row, but the database lease grants
			// execution to exactly one of them.
			type result struct {
				lease ConversationLease
				err   error
			}
			start := make(chan struct{})
			results := make(chan result, 2)
			var ready sync.WaitGroup
			ready.Add(2)
			for i, candidate := range []*PostgresTurnStore{turns, restarted} {
				go func(i int, candidate *PostgresTurnStore) {
					ready.Done()
					<-start
					got, acquireErr := candidate.AcquireConversationLease(ctx, owner, session.ID, first.ID, fmt.Sprintf("replica-%d", i), time.Minute)
					results <- result{lease: got, err: acquireErr}
				}(i, candidate)
			}
			ready.Wait()
			close(start)
			var winner ConversationLease
			successes := 0
			for i := 0; i < 2; i++ {
				outcome := <-results
				if outcome.err == nil {
					successes++
					winner = outcome.lease
				} else {
					assert.True(t, errors.Is(outcome.err, ErrLeaseHeld) || errors.Is(outcome.err, ErrRetryNotDue), outcome.err)
				}
			}
			require.Equal(t, 1, successes)
			lease = winner
		} else {
			lease, err = restarted.AcquireConversationLease(ctx, owner, session.ID, first.ID, fmt.Sprintf("retry-%d", expectedCount), time.Minute)
			require.NoError(t, err)
		}
		retrying, err = restarted.FailTurn(ctx, owner, lease, TurnStatusFailedRetryable, "persistent")
		require.NoError(t, err)
		require.Equal(t, TurnStatusFailedRetryable, retrying.Status)
		require.Equal(t, expectedCount, retrying.RetryCount)
		require.NotNil(t, retrying.NextRetryAt)
		wantDelay := 10 * time.Second
		if expectedCount == 3 {
			wantDelay = 30 * time.Second
		}
		assert.WithinDuration(t, time.Now().Add(wantDelay), *retrying.NextRetryAt, 2*time.Second)
	}

	forceTurnRetryDue(t, db, first.ID)
	lease, err = restarted.AcquireConversationLease(ctx, owner, session.ID, first.ID, "last-recovery", time.Minute)
	require.NoError(t, err)
	final, err := restarted.FailTurn(ctx, owner, lease, TurnStatusFailedRetryable, "persistent")
	require.NoError(t, err)
	assert.Equal(t, TurnStatusFailedFinal, final.Status)
	assert.Equal(t, MaxTurnRecoveryAttempts, final.RetryCount)
	assert.Nil(t, final.NextRetryAt)
	assert.Empty(t, final.ExecutionEnvelope)
	assert.True(t, final.Status.Terminal())
	advisories, err := restarted.ListContinuityAdvisories(ctx, owner, session.ID, 10)
	require.NoError(t, err)
	knownSuccess := false
	for _, advisory := range advisories {
		knownSuccess = knownSuccess || (advisory.Kind == ContinuityAdvisoryKnownSuccess && advisory.TurnID == first.ID)
	}
	assert.True(t, knownSuccess, "a known successful action must survive answer-retry exhaustion as context")

	var failedMessages int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM messages WHERE turn_id = $1 AND status = 'error'`, first.ID).Scan(&failedMessages))
	assert.Equal(t, 2, failedMessages)
	unchanged, err := sessions.GetByID(ctx, owner, session.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, unchanged.ContextVersion, "retry failure must never overwrite session context")

	nextLease, err := restarted.AcquireConversationLease(ctx, owner, session.ID, queued.ID, "next-turn", time.Minute)
	require.NoError(t, err, "a terminally exhausted head must release the session queue")
	_, err = restarted.FailTurn(ctx, owner, nextLease, TurnStatusAborted, "test_cleanup")
	require.NoError(t, err)
}

func TestPostgresTurnStore_RecoveryEnvelopeAndOrphanScan(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 81101, OrganizationID: 81102}
	other := Owner{TopOrganizationID: 81101, OrganizationID: 81999}
	session, err := NewSessionStore(db).Create(ctx, owner, nil, nil)
	require.NoError(t, err)
	turns := NewPostgresTurnStore(db)
	envelope := json.RawMessage(`{"version":1,"message":"resume me"}`)
	turn, _, err := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
		SessionID: session.ID, ClientTurnID: "recoverable", RequestHash: HashTurnRequest("recoverable"),
		UserContent: "resume me", ExecutionEnvelope: envelope,
	})
	require.NoError(t, err)
	assert.JSONEq(t, string(envelope), string(turn.ExecutionEnvelope))
	stored, err := turns.GetExecutionEnvelope(ctx, owner, turn.ID)
	require.NoError(t, err)
	assert.JSONEq(t, string(envelope), string(stored))
	_, err = turns.GetExecutionEnvelope(ctx, other, turn.ID)
	require.ErrorIs(t, err, ErrTurnNotFound)

	recoverable, err := turns.ListRecoverableTurns(ctx, 10)
	require.NoError(t, err)
	require.Len(t, recoverable, 1)
	assert.Equal(t, turn.ID, recoverable[0].Turn.ID)
	assert.Equal(t, owner, recoverable[0].Turn.Owner)

	lease, err := turns.AcquireConversationLease(ctx, owner, session.ID, turn.ID, "old-replica", 5*time.Second)
	require.NoError(t, err)
	recoverable, err = turns.ListRecoverableTurns(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, recoverable, "a healthy active executor must not be stolen")
	_, err = db.Exec(`UPDATE conversation_leases SET lease_until = NOW() - INTERVAL '1 second' WHERE session_id = $1`, lease.SessionID)
	require.NoError(t, err)
	recoverable, err = turns.ListRecoverableTurns(ctx, 10)
	require.NoError(t, err)
	require.Len(t, recoverable, 1)
	assert.Equal(t, TurnStatusRunning, recoverable[0].Turn.Status)
	retryLease, err := turns.AcquireConversationLease(ctx, owner, session.ID, turn.ID, "recovery-replica", time.Minute)
	require.NoError(t, err)
	retryable, err := turns.FailTurn(ctx, owner, retryLease, TurnStatusFailedRetryable, "temporary_failure")
	require.NoError(t, err)
	assert.NotEmpty(t, retryable.ExecutionEnvelope, "retryable turns must retain the only restart input")
}

func TestPostgresTurnStore_AdvisoryProjectionAndLateOutcome(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 81201, OrganizationID: 81202}
	other := Owner{TopOrganizationID: 81201, OrganizationID: 81299}
	session, err := NewSessionStore(db).Create(ctx, owner, nil, nil)
	require.NoError(t, err)
	turns := NewPostgresTurnStore(db)
	turn, _, err := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
		SessionID: session.ID, ClientTurnID: "late", RequestHash: HashTurnRequest("late"), UserContent: "stop it",
		ExecutionEnvelope: json.RawMessage(`{"version":1,"message":"stop it","client_ip":"203.0.113.8"}`),
	})
	require.NoError(t, err)
	lease, err := turns.AcquireConversationLease(ctx, owner, session.ID, turn.ID, "replica-a", time.Minute)
	require.NoError(t, err)

	_, _, err = turns.ReserveAction(ctx, owner, lease, ReserveActionInput{
		Index: 9, ActionName: "ForbiddenHint", ArgsHash: HashTurnRequest("bad"),
		ContextHint: json.RawMessage(`{"confirmed":true}`),
	})
	require.ErrorIs(t, err, ErrInvalidArgument, "authorization-like fields are not in the hint allowlist")

	hint := json.RawMessage(`{"resource_ids":["uhost-1"],"region":"cn-bj2","zone":"cn-bj2-02"}`)
	action, _, err := turns.ReserveAction(ctx, owner, lease, ReserveActionInput{
		Index: 0, ActionName: "StopCompShareInstance", ArgsHash: HashTurnRequest("stop-uhost-1"), ContextHint: hint,
	})
	require.NoError(t, err)
	action, err = turns.StartAction(ctx, owner, lease, action.ExecutionToken)
	require.NoError(t, err)
	failed, err := turns.FailTurn(ctx, owner, lease, TurnStatusFailedRetryable, "executor_disappeared")
	require.NoError(t, err)
	assert.Equal(t, TurnStatusAmbiguousAfterAction, failed.Status)
	assert.Empty(t, failed.ExecutionEnvelope, "terminal ambiguity must erase the short-lived execution PII")

	requestID := "upstream-request-1"
	_, err = turns.RecordActionWithContext(ctx, other, action.ExecutionToken,
		ActionStatusSucceeded, json.RawMessage(`{"RetCode":0}`), nil, &requestID, hint)
	require.ErrorIs(t, err, ErrActionNotFound)
	known, err := turns.RecordActionWithContext(ctx, owner, action.ExecutionToken,
		ActionStatusSucceeded, json.RawMessage(`{"RetCode":0,"secret":"must-not-project"}`), nil, &requestID, hint)
	require.NoError(t, err)
	assert.Equal(t, ActionStatusSucceeded, known.Status)

	events, err := turns.ListEvents(ctx, owner, turn.ID, 0, 100)
	require.NoError(t, err)
	var late *TurnEvent
	for i := range events {
		if events[i].Type == "action.late_outcome" {
			late = &events[i]
		}
	}
	require.NotNil(t, late)
	assert.False(t, late.Provisional)
	assert.NotContains(t, string(late.Payload), "must-not-project")
	assert.Contains(t, string(late.Payload), "uhost-1")

	advisories, err := turns.ListContinuityAdvisories(ctx, owner, session.ID, 20)
	require.NoError(t, err)
	var kinds []ContinuityAdvisoryKind
	for _, advisory := range advisories {
		kinds = append(kinds, advisory.Kind)
		assert.NotContains(t, string(advisory.ContextHint), "must-not-project")
	}
	assert.Contains(t, kinds, ContinuityAdvisoryAmbiguous)
	assert.Contains(t, kinds, ContinuityAdvisoryKnownSuccess)
	_, err = turns.ListContinuityAdvisories(ctx, other, session.ID, 20)
	require.ErrorIs(t, err, ErrConversationNotFound)

	normal, _, err := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
		SessionID: session.ID, ClientTurnID: "normal-success", RequestHash: HashTurnRequest("normal-success"), UserContent: "normal",
	})
	require.NoError(t, err)
	normalLease, err := turns.AcquireConversationLease(ctx, owner, session.ID, normal.ID, "normal-replica", time.Minute)
	require.NoError(t, err)
	normalAction, _, err := turns.ReserveAction(ctx, owner, normalLease, ReserveActionInput{
		Index: 0, ActionName: "StartCompShareInstance", ArgsHash: HashTurnRequest("normal-action"), ContextHint: hint,
	})
	require.NoError(t, err)
	normalAction, err = turns.StartAction(ctx, owner, normalLease, normalAction.ExecutionToken)
	require.NoError(t, err)
	_, err = turns.RecordActionWithContext(ctx, owner, normalAction.ExecutionToken,
		ActionStatusSucceeded, json.RawMessage(`{"RetCode":0}`), nil, nil, hint)
	require.NoError(t, err)
	_, err = turns.CommitTurn(ctx, owner, CommitTurnInput{
		TurnID: normal.ID, Lease: normalLease, ExpectedContextVersion: 0,
		ContextWriteMode: ContextWritePreserve, Context: json.RawMessage(`null`),
		Assistant:         AssistantPatch{Content: "normal answer"},
		TerminalEventType: "turn.committed", TerminalEventPayload: json.RawMessage(`{"content":"normal answer"}`),
	})
	require.NoError(t, err)
	advisories, err = turns.ListContinuityAdvisories(ctx, owner, session.ID, 20)
	require.NoError(t, err)
	for _, advisory := range advisories {
		assert.False(t, advisory.Kind == ContinuityAdvisoryKnownSuccess && advisory.TurnID == normal.ID,
			"a normally committed action was already explained by its answer and must not be injected again")
	}

	aborted, _, err := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
		SessionID: session.ID, ClientTurnID: "aborted", RequestHash: HashTurnRequest("aborted"), UserContent: "cancel",
		ExecutionEnvelope: json.RawMessage(`{"version":1,"message":"cancel","client_ip":"203.0.113.9"}`),
	})
	require.NoError(t, err)
	abortLease, err := turns.AcquireConversationLease(ctx, owner, session.ID, aborted.ID, "replica-b", time.Minute)
	require.NoError(t, err)
	aborted, err = turns.FailTurn(ctx, owner, abortLease, TurnStatusAborted, "client_aborted")
	require.NoError(t, err)
	assert.Empty(t, aborted.ExecutionEnvelope)
	advisories, err = turns.ListContinuityAdvisories(ctx, owner, session.ID, 20)
	require.NoError(t, err)
	foundAborted := false
	for _, advisory := range advisories {
		foundAborted = foundAborted || advisory.Kind == ContinuityAdvisoryAborted
	}
	assert.True(t, foundAborted)
}

func TestPostgresTurnStore_ContextHintDefenseIsEnforcedBySQLAndReadBoundary(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 81301, OrganizationID: 81302}
	session, err := NewSessionStore(db).Create(ctx, owner, nil, nil)
	require.NoError(t, err)
	turns := NewPostgresTurnStore(db)
	turn, _, err := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
		SessionID: session.ID, ClientTurnID: "hint-defense", RequestHash: HashTurnRequest("hint-defense"), UserContent: "change it",
	})
	require.NoError(t, err)
	lease, err := turns.AcquireConversationLease(ctx, owner, session.ID, turn.ID, "hint-replica", time.Minute)
	require.NoError(t, err)
	action, _, err := turns.ReserveAction(ctx, owner, lease, ReserveActionInput{
		Index: 0, ActionName: "StopCompShareInstance", ArgsHash: HashTurnRequest("hint-action"),
		ContextHint: json.RawMessage(`{"resource_ids":["uhost-1"]}`),
	})
	require.NoError(t, err)
	action, err = turns.StartAction(ctx, owner, lease, action.ExecutionToken)
	require.NoError(t, err)
	failed, err := turns.FailTurn(ctx, owner, lease, TurnStatusFailedRetryable, "lost_after_action")
	require.NoError(t, err)
	require.Equal(t, TurnStatusAmbiguousAfterAction, failed.Status)
	_, err = turns.RecordActionWithContext(ctx, owner, action.ExecutionToken, ActionStatusSucceeded,
		json.RawMessage(`{"RetCode":0}`), nil, nil, json.RawMessage(`{"resource_ids":["uhost-1"]}`))
	require.NoError(t, err)

	_, err = db.Exec(`UPDATE turn_actions SET context_hint = '{"resource_ids":[{"confirmed":true}]}'::jsonb WHERE execution_token = $1`, action.ExecutionToken)
	require.Error(t, err, "PostgreSQL must reject non-string resource_ids even from a direct writer")

	// A structurally valid direct write may still be non-canonical. The prompt
	// projection validates and canonicalizes it again instead of trusting storage.
	_, err = db.Exec(`UPDATE turn_actions SET context_hint = '{"resource_ids":[" uhost-1 ","uhost-1"]}'::jsonb WHERE execution_token = $1`, action.ExecutionToken)
	require.NoError(t, err)
	advisories, err := turns.ListContinuityAdvisories(ctx, owner, session.ID, 10)
	require.NoError(t, err)
	var known *ContinuityAdvisory
	for i := range advisories {
		if advisories[i].Kind == ContinuityAdvisoryKnownSuccess {
			known = &advisories[i]
		}
	}
	require.NotNil(t, known)
	assert.JSONEq(t, `{"resource_ids":["uhost-1"]}`, string(known.ContextHint))
}

func TestPostgresTurnStore_ContinuityAdvisoriesAreBoundedByTurnsAndTime(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 81401, OrganizationID: 81402}
	session, err := NewSessionStore(db).Create(ctx, owner, nil, nil)
	require.NoError(t, err)
	turns := NewPostgresTurnStore(db)
	turnIDs := make([]string, 0, 11)
	for i := 1; i <= 11; i++ {
		turn, _, acceptErr := turns.AcceptTurn(ctx, owner, AcceptTurnInput{
			SessionID: session.ID, ClientTurnID: fmt.Sprintf("bounded-%02d", i),
			RequestHash: HashTurnRequest(fmt.Sprintf("bounded-%02d", i)), UserContent: "cancel",
		})
		require.NoError(t, acceptErr)
		lease, acquireErr := turns.AcquireConversationLease(ctx, owner, session.ID, turn.ID, fmt.Sprintf("bounded-replica-%02d", i), time.Minute)
		require.NoError(t, acquireErr)
		turn, failErr := turns.FailTurn(ctx, owner, lease, TurnStatusAborted, "client_aborted")
		require.NoError(t, failErr)
		turnIDs = append(turnIDs, turn.ID)
	}

	advisories, err := turns.ListContinuityAdvisories(ctx, owner, session.ID, 200)
	require.NoError(t, err)
	require.Len(t, advisories, 10)
	for _, advisory := range advisories {
		assert.NotEqual(t, turnIDs[0], advisory.TurnID, "the eleventh-oldest turn is outside the ten-turn window")
	}

	latestID := turnIDs[len(turnIDs)-1]
	_, err = db.Exec(`UPDATE chat_turns SET updated_at = NOW() - INTERVAL '25 hours', finished_at = NOW() - INTERVAL '25 hours' WHERE id = $1`, latestID)
	require.NoError(t, err)
	advisories, err = turns.ListContinuityAdvisories(ctx, owner, session.ID, 10)
	require.NoError(t, err)
	require.Len(t, advisories, 9)
	for _, advisory := range advisories {
		assert.NotEqual(t, latestID, advisory.TurnID, "advisories older than 24 hours must not reach the agent")
	}
}
