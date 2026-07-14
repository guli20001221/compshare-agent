package store

import (
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

func TestActionStatusMayHaveExecuted(t *testing.T) {
	assert.True(t, ActionStatusReserved.MayHaveExecuted())
	assert.True(t, ActionStatusSucceeded.MayHaveExecuted())
	assert.True(t, ActionStatusAmbiguous.MayHaveExecuted())
	assert.False(t, ActionStatusFailed.MayHaveExecuted())
}
