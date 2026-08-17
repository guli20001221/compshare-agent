package turntrace

import (
	"context"
	"testing"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type captureWriter struct{ records []observability.TraceRecord }

func (w *captureWriter) Append(record observability.TraceRecord) error {
	w.records = append(w.records, record)
	return nil
}
func (*captureWriter) Dir() string                 { return "" }
func (*captureWriter) Close(context.Context) error { return nil }

func TestRecorderHooksCoverCompleteEngineSurface(t *testing.T) {
	writer := &captureWriter{}
	recorder := New(Config{Writer: writer, TraceID: "turn:e1", TurnID: "turn", TurnIndex: 2, Start: time.Now()})
	hooks := recorder.Hooks()
	require.NotNil(t, hooks.Retrieval)
	require.NotNil(t, hooks.Freshness)
	require.NotNil(t, hooks.Diagnosis)
	require.NotNil(t, hooks.Outcome)
	require.NotNil(t, hooks.Renderer)
	require.NotNil(t, hooks.HardBlock)
	require.NotNil(t, hooks.Completion)
	require.NotNil(t, hooks.RateLimit)
	require.NotNil(t, hooks.TokenUsage)
	require.NotNil(t, hooks.Confirmation)

	hooks.Retrieval(observability.RetrievalTrace{Enabled: true, Hits: 1})
	hooks.Freshness(observability.FreshnessTrace{MonitorCallInCurrentTurn: true})
	hooks.Diagnosis(observability.DiagnosisTrace{Claims: []observability.DiagnosisClaimTrace{{Claim: "checked", Status: "supported"}}})
	hooks.Outcome(observability.OutcomeTrace{AttemptedHallucinatedCount: 1})
	hooks.Renderer(observability.RendererTrace{Enabled: true, Status: "ok"})
	hooks.HardBlock(observability.EngineHardBlockTrace{})
	hooks.Completion(observability.TurnCompletionTrace{Class: observability.CompletionClassAgent, Reason: observability.CompletionReasonAgentLoop, ModelCalls: 1})
	hooks.RateLimit(governance.Decision{Allowed: true})
	hooks.TokenUsage(llm.TokenUsage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7})
	hooks.Confirmation(observability.ConfirmationTrace{Action: "CreateInstanceWorkflow", State: observability.ConfirmationStateConfirmed, TerminalReason: observability.ConfirmationReasonUserConfirmed})
	recorder.OnStep(engine.StepEvent{Type: engine.StepToolCall, Action: "ReadOnly", Args: map[string]any{"UHostId": "uhost-1"}})
	recorder.OnStep(engine.StepEvent{Type: engine.StepToolResult, Action: "ReadOnly", TraceResult: map[string]any{"ok": true}, Projected: true})
	require.NoError(t, recorder.Finish(nil, nil, "answer", engine.TraceSnapshot{SessionStateHydrated: true}, time.Now()))
	require.Len(t, writer.records, 1)
	got := writer.records[0]
	assert.True(t, got.Retrieval.Enabled)
	assert.True(t, got.Freshness.MonitorCallInCurrentTurn)
	require.Len(t, got.Diagnosis.Claims, 1)
	assert.True(t, got.Renderer.Enabled)
	assert.Equal(t, observability.CompletionClassAgent, got.Completion.Class)
	assert.True(t, got.RateLimit.Checked)
	require.Len(t, got.ToolCalls, 1)
	assert.Equal(t, observability.ToolStatusSuccess, got.ToolCalls[0].Status)
	assert.True(t, got.ToolCalls[0].Projected, "the durable trace must say the model saw a projected tool result")
	require.Len(t, got.Confirmations, 1)
}

// Turn-start provenance must survive separately from the final session state:
// a new SSH entry card can legitimately re-bind an expired user selection to a
// fresh one later in the same turn. Without both boundaries, a trace makes the
// intentional expired-selection gate look like lost conversation context.
func TestRecorderCarriesInstanceSelectionProvenanceAtBothTurnBoundaries(t *testing.T) {
	writer := &captureWriter{}
	recorder := New(Config{Writer: writer, TraceID: "turn:selection", TurnID: "turn", TurnIndex: 1, Start: time.Now()})
	snapshot := engine.TraceSnapshot{
		SessionStateHydrated: true,
		SessionState: engine.SessionState{
			SelectedInstanceID:        "uhost-target",
			SelectedInstanceSource:    engine.SelectedInstanceSourceUser,
			SelectedInstanceFreshness: engine.ContinuityFreshnessFresh,
		},
		SelectedInstanceIDAtStart:        "uhost-target",
		SelectedInstanceSourceAtStart:    engine.SelectedInstanceSourceUser,
		SelectedInstanceFreshnessAtStart: engine.ContinuityFreshnessExpired,
	}
	require.NoError(t, recorder.Finish(nil, nil, "answer", snapshot, time.Now()))
	require.Len(t, writer.records, 1)
	state := writer.records[0].State
	assert.Equal(t, "uhost-target", state.SelectedInstanceID)
	assert.Equal(t, engine.SelectedInstanceSourceUser, state.SelectedInstanceSource)
	assert.Equal(t, engine.ContinuityFreshnessFresh, state.SelectedInstanceFreshness)
	assert.Equal(t, "uhost-target", state.SelectedInstanceIDAtTurnStart)
	assert.Equal(t, engine.SelectedInstanceSourceUser, state.SelectedInstanceSourceAtTurnStart)
	assert.Equal(t, engine.ContinuityFreshnessExpired, state.SelectedInstanceFreshnessAtTurnStart)
}

func TestRecorderDoesNotPersistFreeTextErrorsOrContinuityReasons(t *testing.T) {
	writer := &captureWriter{}
	secret := "password=hunter2 sql=SELECT-secret"
	recorder := New(Config{Writer: writer, TraceID: "turn:e1", TurnID: "turn", TurnIndex: 1, Start: time.Now()})
	recorder.OnStep(engine.StepEvent{Type: engine.StepError, Action: "ReadOnly", Message: secret})
	recorder.SetContinuity(func(trace *observability.ContinuityTrace) {
		trace.CommitOutcome = "failed_final"
		trace.CommitReason = secret
	})
	require.NoError(t, recorder.Finish(nil, assert.AnError, "", engine.TraceSnapshot{}, time.Now()))
	require.Len(t, writer.records, 1)
	got := writer.records[0]
	require.Len(t, got.ToolCalls, 1)
	assert.Equal(t, "tool_error", got.ToolCalls[0].ErrorClass)
	assert.Equal(t, "other", got.Continuity.CommitReason)
	assert.False(t, got.EngineHardBlock.Hit, "commit/protocol errors are not engine policy blocks")
	assert.NotContains(t, got.ToolCalls[0].ErrorClass, "hunter2")
	assert.NotContains(t, got.Continuity.CommitReason, "hunter2")
}

func TestRecorderMeasuresLatencyForPairedToolResultsAndErrors(t *testing.T) {
	writer := &captureWriter{}
	start := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	recorder := New(Config{Writer: writer, TraceID: "turn:e1", TurnID: "turn", TurnIndex: 1, Start: start})
	recorder.now = func() time.Time { return start }
	recorder.OnStep(engine.StepEvent{Type: engine.StepToolCall, Action: "ReadSucceeded"})
	start = start.Add(1200 * time.Millisecond)
	recorder.OnStep(engine.StepEvent{Type: engine.StepToolResult, Action: "ReadSucceeded"})
	recorder.OnStep(engine.StepEvent{Type: engine.StepToolCall, Action: "ReadFailed"})
	start = start.Add(2400 * time.Millisecond)
	recorder.OnStep(engine.StepEvent{Type: engine.StepError, Action: "ReadFailed", Message: "raw upstream detail"})
	require.NoError(t, recorder.Finish(nil, nil, "", engine.TraceSnapshot{}, start))

	require.Len(t, writer.records, 1)
	require.Len(t, writer.records[0].ToolCalls, 2)
	require.NotNil(t, writer.records[0].ToolCalls[0].LatencyMS, "a paired success must carry observed latency")
	assert.Equal(t, int64(1200), *writer.records[0].ToolCalls[0].LatencyMS)
	assert.Equal(t, observability.ToolStatusSuccess, writer.records[0].ToolCalls[0].Status)
	require.NotNil(t, writer.records[0].ToolCalls[1].LatencyMS, "a paired failure must carry observed latency")
	assert.Equal(t, int64(2400), *writer.records[0].ToolCalls[1].LatencyMS)
	assert.Equal(t, "tool_error", writer.records[0].ToolCalls[1].ErrorClass)
}

// Durable recovery can receive an outcome without its original call event.
// Preserve that lack of observation as nil rather than corrupting latency
// percentiles with a synthetic 0ms duration.
func TestRecorderLeavesLatencyUnsetForUnpairedEvents(t *testing.T) {
	writer := &captureWriter{}
	start := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	recorder := New(Config{Writer: writer, TraceID: "turn:orphan", TurnID: "turn", TurnIndex: 1, Start: start})
	recorder.OnStep(engine.StepEvent{Type: engine.StepToolResult, Action: "OrphanSuccess"})
	recorder.OnStep(engine.StepEvent{Type: engine.StepError, Action: "OrphanFailure", Message: "upstream failed"})
	require.NoError(t, recorder.Finish(nil, nil, "", engine.TraceSnapshot{}, start))

	require.Len(t, writer.records, 1)
	require.Len(t, writer.records[0].ToolCalls, 2)
	assert.Nil(t, writer.records[0].ToolCalls[0].LatencyMS, "unpaired success must not be reported as 0ms")
	assert.Nil(t, writer.records[0].ToolCalls[1].LatencyMS, "unpaired error must not be reported as 0ms")
}

func TestRecorderPersistsContinuityContractMetadataAndMarksFailures(t *testing.T) {
	writer := &captureWriter{}
	recorder := New(Config{Writer: writer, TraceID: "turn:e1", TurnID: "turn", TurnIndex: 1, Start: time.Now()})
	snapshot := engine.TraceSnapshot{
		ContextSources:     []string{"recent_pairs", "selected_entities"},
		ResponseContract:   "grounded",
		PromptSectionIDs:   []string{"identity", "knowledge_turn_policy"},
		MemoryUpdateSource: "structured_event",
		GroundingOutcome:   "supported",
	}
	require.NoError(t, recorder.Finish(nil, assert.AnError, "", snapshot, time.Now()))
	require.Len(t, writer.records, 1)
	got := writer.records[0].Outcome
	assert.Equal(t, "failure", got.ResponseContract)
	assert.Equal(t, snapshot.ContextSources, got.ContextSources)
	assert.Equal(t, snapshot.PromptSectionIDs, got.PromptSectionIDs)
	assert.Equal(t, snapshot.MemoryUpdateSource, got.MemoryUpdateSource)
	assert.Equal(t, snapshot.GroundingOutcome, got.GroundingOutcome)
}
