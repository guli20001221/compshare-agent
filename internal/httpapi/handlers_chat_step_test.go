package httpapi

import (
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDispatchChat_EmitsStepEvents verifies that tool calls during the ReAct
// loop produce event:step SSE frames with the projected stepEvent fields.
func TestDispatchChat_EmitsStepEvents(t *testing.T) {
	llmFake := &toolTurnLLM{}
	eng := engine.NewWithDeps(llmFake, toolTurnExecutor{}, denyConfirm)
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

	sink, _ := dispatchChatTurn(t, h, sess.ID, "show instances")

	body := sink.body()

	assert.True(t, sink.has("step"), "expected at least one step event")
	assert.Contains(t, body, `"Type":"tool_call"`)
	assert.Contains(t, body, `"Type":"tool_result"`)
	assert.Contains(t, body, `"Action":"ReadCapability_resource_info"`)

	// Args must NOT leak into step events (they contain API parameters).
	assert.NotContains(t, body, `"Limit"`)
	assert.NotContains(t, body, `TraceResult`)
	assert.NotContains(t, body, `Display`)

	// Tool results must also stay out of frontend step events. The trace may
	// hash them, but the streamed frames should only show coarse progress.
	assert.NotContains(t, body, "uhost-e2e")
	assert.NotContains(t, body, "e2e-host")
	assert.NotContains(t, body, "RTX4090")

	// Message must be present for tool_result steps (engine emits "调用成功").
	assert.Contains(t, body, `"Message":"调用成功"`)

	// Index should increment: Index 0 must appear before Index 1.
	assert.Contains(t, body, `"Index":0`)
	assert.Contains(t, body, `"Index":1`)
	idx0 := strings.Index(body, `"Index":0`)
	idx1 := strings.Index(body, `"Index":1`)
	assert.Less(t, idx0, idx1, "Index 0 must appear before Index 1")

	// Standard events must still be present.
	assert.True(t, sink.has("meta"))
	assert.True(t, sink.has("token"))
	assert.True(t, sink.has("done"))
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
	llmFake := &toolTurnLLM{}
	eng := engine.NewWithDeps(llmFake, toolTurnExecutor{}, denyConfirm)
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

	sink, _ := dispatchChatTurn(t, h, sess.ID, "list instances")

	lastStep := sink.lastIndexOf("step")
	firstDone := sink.firstIndexOf("done")
	require.Greater(t, lastStep, -1, "must have at least one step event")
	require.Greater(t, firstDone, -1, "must have done event")
	assert.Less(t, lastStep, firstDone, "all step events must precede done")
}

func TestDispatchChatTraceRecordsToolCall(t *testing.T) {
	llmFake := &toolTurnLLM{}
	eng := engine.NewWithDeps(llmFake, toolTurnExecutor{}, denyConfirm)
	eng.RehydrateHistory(nil)

	sess := store.Session{
		ID:                "sess-runtime-form",
		TopOrganizationID: 1,
		OrganizationID:    2,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}
	sessions := &mockSessions{byID: map[string]store.Session{sess.ID: sess}}
	traceWriter := &captureTraceWriter{}
	h := NewHandlers(
		&config.Config{Agent: config.AgentConfig{
			LLM:  config.LLMConfig{Model: "model-x"},
			HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: time.Hour},
			STS:  config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
		}},
		sessions,
		&recordingMessages{},
		mockFeedback{},
		fakePool{eng: eng},
		traceWriter,
	)

	_, _ = dispatchChatTurn(t, h, sess.ID, "show instances")

	require.Len(t, traceWriter.records, 1)
	trace := traceWriter.records[0]
	require.Len(t, trace.ToolCalls, 2)
	assert.Equal(t, "ReadCapability_resource_info", trace.ToolCalls[0].Action)
	assert.Equal(t, "DescribeCompShareInstance", trace.ToolCalls[1].Action)
}
