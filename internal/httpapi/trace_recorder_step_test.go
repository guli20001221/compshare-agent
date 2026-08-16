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

func TestChatTraceRecorderNormalizesToolErrorsAndMeasuresPairedLatency(t *testing.T) {
	w := &captureTraceWriter{}
	start := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	base := BaseRequest{RequestUUID: "req-tool-error", Owner: store.Owner{TopOrganizationID: 1, OrganizationID: 2}}
	rec := newChatTraceRecorder(w, base, "sess-1", 1, "msg", start)
	rec.now = func() time.Time { return start }

	rec.OnStep(engine.StepEvent{
		Type: engine.StepToolCall, Action: "DescribeSucceeded",
		Args: map[string]any{"UHostId": "uhost-demo"},
	})
	start = start.Add(850 * time.Millisecond)
	rec.OnStep(engine.StepEvent{Type: engine.StepToolResult, Action: "DescribeSucceeded"})
	rec.OnStep(engine.StepEvent{Type: engine.StepToolCall, Action: "DescribeFailed"})
	start = start.Add(1350 * time.Millisecond)
	rec.OnStep(engine.StepEvent{
		Type: engine.StepError, Action: "DescribeFailed",
		Message: "upstream credential password=hunter2 timed out",
	})
	require.NoError(t, rec.Finish(nil, start))

	require.Len(t, w.records, 1)
	require.Len(t, w.records[0].ToolCalls, 2)
	success := w.records[0].ToolCalls[0]
	assert.Equal(t, observability.ToolStatusSuccess, success.Status)
	require.NotNil(t, success.LatencyMS, "a paired success must carry observed latency")
	assert.Equal(t, int64(850), *success.LatencyMS)
	call := w.records[0].ToolCalls[1]
	assert.Equal(t, observability.ToolStatusError, call.Status)
	assert.Equal(t, "tool_error", call.ErrorClass)
	require.NotNil(t, call.LatencyMS, "a paired failure must carry observed latency")
	assert.Equal(t, int64(1350), *call.LatencyMS)
	assert.NotContains(t, call.ErrorClass, "hunter2")
}

// A result/error without an observed call exists in compatibility and recovery
// paths. It must remain visibly unpaired, not look like a real 0ms tool call.
func TestChatTraceRecorderLeavesLatencyUnsetForUnpairedEvents(t *testing.T) {
	w := &captureTraceWriter{}
	start := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	base := BaseRequest{RequestUUID: "req-orphan-tool", Owner: store.Owner{TopOrganizationID: 1, OrganizationID: 2}}
	rec := newChatTraceRecorder(w, base, "sess-1", 1, "msg", start)
	rec.OnStep(engine.StepEvent{Type: engine.StepToolResult, Action: "OrphanSuccess"})
	rec.OnStep(engine.StepEvent{Type: engine.StepError, Action: "OrphanFailure", Message: "upstream failed"})
	require.NoError(t, rec.Finish(nil, start))

	require.Len(t, w.records, 1)
	require.Len(t, w.records[0].ToolCalls, 2)
	assert.Nil(t, w.records[0].ToolCalls[0].LatencyMS, "unpaired success must not be reported as 0ms")
	assert.Nil(t, w.records[0].ToolCalls[1].LatencyMS, "unpaired error must not be reported as 0ms")
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

// TestChatTraceRecorder_UserNoticeIsNotAToolEvent pins the boundary that keeps the interruption
// notice (internal/engine/instance_ops_interruption.go) out of the tool counters.
//
// The notice reports what a PREVIOUS turn's diagnosis did. On the turn that shows it, no tool was
// called, none failed and none was blocked. Sent as StepBlocked — which is what it was, before this
// — the recorder synthesized a ToolCallTrace for a tool that never ran and stamped it
// error/blocked, so every turn after an interruption read as a turn where a tool had been refused.
// Tool error counts are what an incident is triaged from, so that is a false signal, not a cosmetic
// one.
func TestChatTraceRecorder_UserNoticeIsNotAToolEvent(t *testing.T) {
	w := &captureTraceWriter{}
	base := BaseRequest{RequestUUID: "req-notice", Owner: store.Owner{TopOrganizationID: 1, OrganizationID: 2}}
	rec := newChatTraceRecorder(w, base, "sess-1", 1, "msg", time.Now())
	require.NotNil(t, rec)

	rec.OnStep(engine.StepEvent{
		Type:    engine.StepUserNotice,
		Action:  "InstanceOpsInterrupted",
		Source:  observability.ToolSourceDiagnosisInternal,
		Message: "上一轮对实例 uhost-1 的实例内排查没有正常结束。",
	})

	assert.Empty(t, rec.record.ToolCalls, "a user notice must not create a tool call trace")

	// ...and a genuinely blocked tool on the same recorder still records, so this is a carve-out
	// for one step TYPE rather than for the diagnosis source or a name.
	rec.OnStep(engine.StepEvent{
		Type:   engine.StepBlocked,
		Action: "DiagnoseInstanceInternals",
		Source: observability.ToolSourceDiagnosisInternal,
	})
	require.Len(t, rec.record.ToolCalls, 1)
	assert.Equal(t, "blocked", rec.record.ToolCalls[0].ErrorClass)
}

// The wire word is its own, not "blocked": a client counting errors must not count it.
func TestStepTypeStringNamesTheUserNotice(t *testing.T) {
	assert.Equal(t, "user_notice", stepTypeString(engine.StepUserNotice))
	assert.Equal(t, "blocked", stepTypeString(engine.StepBlocked))
}
