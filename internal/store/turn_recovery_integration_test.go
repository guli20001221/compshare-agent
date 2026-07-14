package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
