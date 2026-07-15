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
func (*captureWriter) EmitStep(observability.StepTrace) error { return nil }
func (*captureWriter) Dir() string                            { return "" }
func (*captureWriter) Close(context.Context) error            { return nil }

func TestRecorderHooksCoverCompleteEngineSurface(t *testing.T) {
	writer := &captureWriter{}
	recorder := New(Config{Writer: writer, TraceID: "turn:e1", TurnID: "turn", TurnIndex: 2, Start: time.Now()})
	hooks := recorder.Hooks()
	require.NotNil(t, hooks.Planner)
	require.NotNil(t, hooks.Context)
	require.NotNil(t, hooks.Retrieval)
	require.NotNil(t, hooks.Freshness)
	require.NotNil(t, hooks.Diagnosis)
	require.NotNil(t, hooks.Outcome)
	require.NotNil(t, hooks.Renderer)
	require.NotNil(t, hooks.HardBlock)
	require.NotNil(t, hooks.Completion)
	require.NotNil(t, hooks.RateLimit)
	require.NotNil(t, hooks.TokenUsage)
	require.NotNil(t, hooks.StepSink)

	hooks.Planner(observability.RouterTrace{RouteStatus: "dispatched_agent", InputTokens: 2, OutputTokens: 1})
	hooks.Context(observability.ContextTrace{LoopMessages: 3})
	hooks.Retrieval(observability.RetrievalTrace{Enabled: true, Hits: 1})
	hooks.Freshness(observability.FreshnessTrace{MonitorCallInCurrentTurn: true})
	hooks.Diagnosis(observability.DiagnosisTrace{Claims: []observability.DiagnosisClaimTrace{{Claim: "checked", Status: "supported"}}})
	hooks.Outcome(observability.OutcomeTrace{AttemptedHallucinatedCount: 1})
	hooks.Renderer(observability.RendererTrace{Enabled: true, Status: "ok"})
	hooks.HardBlock(observability.EngineHardBlockTrace{})
	hooks.Completion(observability.TurnCompletionTrace{Class: observability.CompletionClassAgent, Reason: observability.CompletionReasonAgentLoop, ModelCalls: 1})
	hooks.RateLimit(governance.Decision{Allowed: true})
	hooks.TokenUsage(llm.TokenUsage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7})
	recorder.OnStep(engine.StepEvent{Type: engine.StepToolCall, Action: "ReadOnly", Args: map[string]any{"UHostId": "uhost-1"}})
	recorder.OnStep(engine.StepEvent{Type: engine.StepToolResult, Action: "ReadOnly", TraceResult: map[string]any{"ok": true}})
	require.NoError(t, hooks.StepSink.EmitStep(observability.StepTrace{StepID: "s1", State: observability.StepStateSuccess}))
	require.NoError(t, recorder.Finish(nil, nil, "answer", engine.TraceSnapshot{SessionStateHydrated: true}, time.Now()))
	require.Len(t, writer.records, 1)
	got := writer.records[0]
	assert.Equal(t, 3, got.Context.LoopMessages)
	assert.True(t, got.Retrieval.Enabled)
	assert.True(t, got.Freshness.MonitorCallInCurrentTurn)
	require.Len(t, got.Diagnosis.Claims, 1)
	assert.True(t, got.Renderer.Enabled)
	assert.Equal(t, observability.CompletionClassAgent, got.Completion.Class)
	assert.True(t, got.RateLimit.Checked)
	require.Len(t, got.ToolCalls, 1)
	assert.Equal(t, observability.ToolStatusSuccess, got.ToolCalls[0].Status)
	require.Len(t, got.Steps, 1)
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

func TestRecorderPersistsContinuityContractMetadataAndMarksFailures(t *testing.T) {
	writer := &captureWriter{}
	recorder := New(Config{Writer: writer, TraceID: "turn:e1", TurnID: "turn", TurnIndex: 1, Start: time.Now()})
	snapshot := engine.TraceSnapshot{
		ContextSources:     []string{"recent_pairs", "active_task"},
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
