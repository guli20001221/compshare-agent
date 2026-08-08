package httpapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/store"
)

// TestChatTraceRecorder_EmitStepAccumulatesSingleEnqueue locks the HTTP path's
// per-turn contract: workflow steps fold into this turn's record.Steps[] and
// persist with the single Enqueue at Finish — never a
// per-step INSERT (which would collide uk_request_uuid, one agent_traces row
// per turn). *chatTraceRecorder thus satisfies orchestrator.StepSink, and the
// tenant context is carried on the single Enqueue.
func TestChatTraceRecorder_EmitStepAccumulatesSingleEnqueue(t *testing.T) {
	w := &captureTraceWriter{}
	base := BaseRequest{RequestUUID: "req-1", Owner: store.Owner{TopOrganizationID: 1, OrganizationID: 2}}
	rec := newChatTraceRecorder(w, base, "sess-1", 1, "msg", time.Now())
	require.NotNil(t, rec)

	require.NoError(t, rec.EmitStep(observability.StepTrace{StepID: "s0", State: observability.StepStateRunning}))
	require.NoError(t, rec.EmitStep(observability.StepTrace{StepID: "s0", State: observability.StepStateSuccess}))

	require.NoError(t, rec.Finish(nil, time.Now()))

	require.Len(t, w.records, 1, "exactly one Enqueue per turn (no per-step INSERT)")
	require.Len(t, w.records[0].Steps, 2)
	assert.Equal(t, observability.StepStateSuccess, w.records[0].Steps[1].State)
	assert.Equal(t, observability.ExecutionPathAgent, w.records[0].ActualExecutionPath)
	require.Len(t, w.tenants, 1)
	assert.Equal(t, int64(1), w.tenants[0].TopOrgID, "tenant carried on the single Enqueue (Enqueue path, not Append)")
}

// The raw trace payload is deliberately not the model-facing projection. The
// boolean is therefore the trace-side fact that tells a later reader that the
// canonical transcript/model saw a reduced result instead of the hashed source.
func TestChatTraceRecorder_PersistsToolProjection(t *testing.T) {
	w := &captureTraceWriter{}
	base := BaseRequest{RequestUUID: "req-projected", Owner: store.Owner{TopOrganizationID: 1, OrganizationID: 2}}
	rec := newChatTraceRecorder(w, base, "sess-1", 1, "msg", time.Now())
	require.NotNil(t, rec)

	rec.OnStep(engine.StepEvent{Type: engine.StepToolCall, Action: "DescribeCompShareImages"})
	rec.OnStep(engine.StepEvent{
		Type: engine.StepToolResult, Action: "DescribeCompShareImages",
		TraceResult: map[string]any{"raw": true}, Projected: true,
	})
	require.NoError(t, rec.Finish(nil, time.Now()))

	require.Len(t, w.records, 1)
	require.Len(t, w.records[0].ToolCalls, 1)
	assert.True(t, w.records[0].ToolCalls[0].Projected,
		"the HTTP trace must retain that the model saw a projected result")
}

func TestChatTraceRecorderPersistsConfirmationWithoutCardPayload(t *testing.T) {
	w := &captureTraceWriter{}
	base := BaseRequest{RequestUUID: "req-confirm", Owner: store.Owner{TopOrganizationID: 1, OrganizationID: 2}}
	rec := newChatTraceRecorder(w, base, "sess-1", 1, "msg", time.Now())
	rec.AddConfirmationTrace(observability.ConfirmationTrace{
		Action: "CreateInstanceWorkflow", State: observability.ConfirmationStateNotConfirmed,
		TerminalReason: observability.ConfirmationReasonTimeout, ElapsedMS: 123,
	})
	require.NoError(t, rec.Finish(nil, time.Now()))
	require.Len(t, w.records, 1)
	require.Len(t, w.records[0].Confirmations, 1)
	assert.Equal(t, observability.ConfirmationReasonTimeout, w.records[0].Confirmations[0].TerminalReason)
}
