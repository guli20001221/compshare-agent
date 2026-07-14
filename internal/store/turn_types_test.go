package store

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHashTurnRequestIsFramedAndStable(t *testing.T) {
	a := HashTurnRequest("ab", "c")
	b := HashTurnRequest("a", "bc")

	require.Len(t, a, 64)
	assert.Equal(t, a, HashTurnRequest("ab", "c"))
	assert.NotEqual(t, a, b, "length framing must prevent concatenation collisions")
}

func TestTurnStatusTerminalContract(t *testing.T) {
	for _, status := range []TurnStatus{
		TurnStatusCommitted,
		TurnStatusAmbiguousAfterAction,
		TurnStatusAborted,
	} {
		assert.True(t, status.Terminal(), status)
	}
	for _, status := range []TurnStatus{
		TurnStatusAccepted,
		TurnStatusRunning,
		TurnStatusAwaitingConfirmation,
		TurnStatusCommitting,
		TurnStatusFailedRetryable,
	} {
		assert.False(t, status.Terminal(), status)
	}
	assert.False(t, TurnStatus("future_status").Valid())
}

func TestTurnActionMayHaveExecuted(t *testing.T) {
	assert.False(t, (TurnAction{Status: ActionStatusReserved}).MayHaveExecuted())
	assert.True(t, (TurnAction{Status: ActionStatusReserved, InFlight: true}).MayHaveExecuted())
	assert.True(t, (TurnAction{Status: ActionStatusAmbiguous}).MayHaveExecuted())
	assert.False(t, (TurnAction{Status: ActionStatusSucceeded}).MayHaveExecuted(), "known success is replayable, not ambiguous")
	assert.False(t, (TurnAction{Status: ActionStatusFailed}).MayHaveExecuted())
}

func TestHashTurnCommitRequiresAndIncludesContextWriteMode(t *testing.T) {
	base := CommitTurnInput{
		TurnID: "turn-1",
		Lease: ConversationLease{
			SessionID: "session-1",
			TurnID:    "turn-1",
			HolderID:  "replica-1",
			Epoch:     7,
		},
		ExpectedContextVersion: 3,
		Context:                json.RawMessage(`{"schema_version":"1.0"}`),
		Assistant:              AssistantPatch{Content: "answer"},
		TerminalEventType:      "turn.committed",
		TerminalEventPayload:   json.RawMessage(`{"saved":true}`),
	}

	_, err := HashTurnCommit(base)
	require.ErrorIs(t, err, ErrInvalidArgument, "a zero-valued write mode must never choose a context policy implicitly")

	update := base
	update.ContextWriteMode = ContextWriteUpdate
	updateHash, err := HashTurnCommit(update)
	require.NoError(t, err)

	preserve := base
	preserve.ContextWriteMode = ContextWritePreserve
	preserveHash, err := HashTurnCommit(preserve)
	require.NoError(t, err)
	assert.NotEqual(t, updateHash, preserveHash, "commit reconciliation must distinguish updating from preserving context")

	invalid := base
	invalid.ContextWriteMode = ContextWriteMode("future-mode")
	_, err = HashTurnCommit(invalid)
	require.ErrorIs(t, err, ErrInvalidArgument)
}
