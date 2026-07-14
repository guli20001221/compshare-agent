package httpapi

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/turncoord"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCoordinatorStreamEvent_CommittedCarriesCanonicalAndLegacyContract(t *testing.T) {
	created := time.Date(2026, 7, 14, 9, 8, 7, 0, time.UTC)
	payload := json.RawMessage(`{"turn_id":"turn-1","message_id":"assistant-1","content":"保存后的答案","committed":true}`)
	eventName, frame, err := coordinatorStreamEvent(store.Turn{
		ID: "turn-1", SessionID: "session-1", AssistantMessageID: "assistant-1",
	}, turncoord.Event{
		TurnID: "turn-1", Seq: 9, LeaseEpoch: 2, Type: "turn.committed",
		Payload: payload, Provisional: false, CreatedAt: created,
	})
	require.NoError(t, err)
	assert.Equal(t, "done", eventName)
	assert.Equal(t, "turn.committed", frame["Type"])
	assert.Equal(t, "turn.committed", frame["EventType"])
	assert.Equal(t, "turn-1", frame["TurnId"])
	assert.Equal(t, int64(9), frame["Seq"])
	assert.Equal(t, "session-1", frame["SessionId"])
	assert.Equal(t, "保存后的答案", frame["Content"])
	assert.Equal(t, "assistant-1", frame["MessageId"])
	assert.Equal(t, true, frame["Committed"])
	assert.Equal(t, false, frame["Provisional"])
	assert.JSONEq(t, string(payload), string(frame["Payload"].(json.RawMessage)))
}

func TestCoordinatorStreamEvent_InteractionCanBeRebuiltAfterReconnect(t *testing.T) {
	payload := json.RawMessage(`{
		"interaction_key":"confirmation/0",
		"kind":"confirmation",
		"payload":{"action":"StopInstance","summary":{"UHostId":"uhost-1"}}
	}`)
	eventName, frame, err := coordinatorStreamEvent(store.Turn{ID: "turn-2", SessionID: "session-2"}, turncoord.Event{
		TurnID: "turn-2", Seq: 3, Type: "interaction.requested", Payload: payload, Provisional: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "confirmation", eventName)
	assert.Equal(t, "interaction.requested", frame["Type"])
	assert.Equal(t, "confirmation/0", frame["InteractionKey"])
	assert.Equal(t, "confirmation/0", frame["ConfirmationId"])
	assert.Equal(t, "StopInstance", frame["Action"])
	assert.Equal(t, map[string]any{"UHostId": "uhost-1"}, frame["Summary"])
	assert.True(t, frame["Provisional"].(bool))
}

func TestCoordinatorStreamEvent_FailureIsNeverPresentedAsDone(t *testing.T) {
	eventName, frame, err := coordinatorStreamEvent(store.Turn{ID: "turn-3"}, turncoord.Event{
		TurnID: "turn-3", Seq: 6, Type: "turn.failed",
		Payload: json.RawMessage(`{"reason":"turn_not_saved","status":"failed_retryable"}`),
	})
	require.NoError(t, err)
	assert.Equal(t, "error", eventName)
	assert.Equal(t, false, frame["Committed"])
	assert.Equal(t, "failed_retryable", frame["Status"])
	assert.Equal(t, "turn_not_saved", frame["Code"])
	assert.Equal(t, "turn_not_saved", frame["Message"])
}

func TestCoordinatorStreamEvent_RejectsCrossTurnProjection(t *testing.T) {
	_, _, err := coordinatorStreamEvent(store.Turn{ID: "wanted"}, turncoord.Event{
		TurnID: "other", Seq: 1, Type: "turn.running", Payload: json.RawMessage(`{}`),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match")
}
