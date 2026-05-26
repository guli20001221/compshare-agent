package httpapi

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDispatchChat_EmitsStepEvents verifies that tool calls during the ReAct
// loop produce event:step SSE frames with the projected stepEvent fields.
func TestDispatchChat_EmitsStepEvents(t *testing.T) {
	llmFake := &factWritingLLM{}
	eng := engine.NewWithDeps(llmFake, factWritingExecutor{}, denyConfirm)
	eng.RehydrateHistory(nil)

	sess := store.Session{
		ID:                "sess-step",
		TopOrganizationID: 1,
		OrganizationID:    2,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
	sessions := &mockSessions{byID: map[string]store.Session{sess.ID: sess}}
	h := newChatTestHandlersWith(t, eng, sessions)

	rec := dispatchChatTurn(t, h, sess.ID, "show instances")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	assert.Contains(t, body, "event: step", "expected at least one step event")
	assert.Contains(t, body, `"Type":"tool_call"`)
	assert.Contains(t, body, `"Type":"tool_result"`)
	assert.Contains(t, body, `"Action":"DescribeCompShareInstance"`)

	// Args must NOT leak into step events (they contain API parameters).
	assert.NotContains(t, body, `"Limit"`)

	// Message must be present for tool_result steps (engine emits "调用成功").
	assert.Contains(t, body, `"Message":"调用成功"`)

	// Index should increment: Index 0 must appear before Index 1.
	assert.Contains(t, body, `"Index":0`)
	assert.Contains(t, body, `"Index":1`)
	idx0 := strings.Index(body, `"Index":0`)
	idx1 := strings.Index(body, `"Index":1`)
	assert.Less(t, idx0, idx1, "Index 0 must appear before Index 1")

	// Standard events must still be present.
	assert.Contains(t, body, "event: meta")
	assert.Contains(t, body, "event: token")
	assert.Contains(t, body, "event: done")
}

// TestStepTypeString covers the stepTypeString mapping.
func TestStepTypeString(t *testing.T) {
	cases := []struct {
		in   engine.StepType
		want string
	}{
		{engine.StepToolCall, "tool_call"},
		{engine.StepToolResult, "tool_result"},
		{engine.StepConfirmNeeded, "confirm_needed"},
		{engine.StepBlocked, "blocked"},
		{engine.StepError, "error"},
		{engine.StepType(99), "unknown"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, stepTypeString(tc.in))
	}
}

// TestDispatchChat_StepEventsAppearBeforeDone verifies ordering: all step
// events must precede the done event in the SSE stream.
func TestDispatchChat_StepEventsAppearBeforeDone(t *testing.T) {
	llmFake := &factWritingLLM{}
	eng := engine.NewWithDeps(llmFake, factWritingExecutor{}, denyConfirm)
	eng.RehydrateHistory(nil)

	sess := store.Session{
		ID:                "sess-order",
		TopOrganizationID: 1,
		OrganizationID:    2,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
	sessions := &mockSessions{byID: map[string]store.Session{sess.ID: sess}}
	h := newChatTestHandlersWith(t, eng, sessions)

	rec := dispatchChatTurn(t, h, sess.ID, "list instances")
	body := rec.Body.String()

	lastStep := strings.LastIndex(body, "event: step")
	firstDone := strings.Index(body, "event: done")
	require.Greater(t, lastStep, -1, "must have at least one step event")
	require.Greater(t, firstDone, -1, "must have done event")
	assert.Less(t, lastStep, firstDone, "all step events must precede done")
}
