package engine

import (
	"testing"

	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/stretchr/testify/require"
)

type traceHookStepSink struct{}

func (traceHookStepSink) EmitStep(observability.StepTrace) error { return nil }

func TestAttachTraceHooksWiresEveryEngineObserver(t *testing.T) {
	eng := &Engine{}
	sink := traceHookStepSink{}
	eng.AttachTraceHooks(TraceHooks{
		Planner: func(observability.RouterTrace) {}, Context: func(observability.ContextTrace) {},
		Retrieval: func(observability.RetrievalTrace) {}, Freshness: func(observability.FreshnessTrace) {},
		Diagnosis: func(observability.DiagnosisTrace) {}, Outcome: func(observability.OutcomeTrace) {},
		Renderer: func(observability.RendererTrace) {}, HardBlock: func(observability.EngineHardBlockTrace) {},
		ContextDecision: func(ContextDecisionTrace) {}, Completion: func(observability.TurnCompletionTrace) {},
		RateLimit: func(governance.Decision) {}, TokenUsage: func(llm.TokenUsage) {}, StepSink: sink,
	})
	require.NotNil(t, eng.plannerTraceObserver)
	require.NotNil(t, eng.contextTraceObserver)
	require.NotNil(t, eng.retrievalTraceObserver)
	require.NotNil(t, eng.freshnessTraceObserver)
	require.NotNil(t, eng.diagnosisTraceObserver)
	require.NotNil(t, eng.outcomeTraceObserver)
	require.NotNil(t, eng.rendererTraceObserver)
	require.NotNil(t, eng.hardBlockObserver)
	require.NotNil(t, eng.contextDecisionObserver)
	require.NotNil(t, eng.turnCompletionObserver)
	require.NotNil(t, eng.rateLimitObserver)
	require.NotNil(t, eng.tokenUsageObserver)
	require.Equal(t, sink, eng.stepSink)
}
