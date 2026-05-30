package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/workflow"
)

type sagaFakeExecutor struct{ calls []string }

func (f *sagaFakeExecutor) Execute(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	f.calls = append(f.calls, action)
	return map[string]any{"RetCode": float64(0)}, nil
}

type sagaFakeSink struct{ steps []observability.StepTrace }

func (s *sagaFakeSink) EmitStep(st observability.StepTrace) error {
	s.steps = append(s.steps, st)
	return nil
}

func (s *sagaFakeSink) hasState(state observability.StepState) bool {
	for _, st := range s.steps {
		if st.State == state {
			return true
		}
	}
	return false
}

// TestEngine_RunAgentSaga_DrivesSaga proves the B8.2 engine seam: RunAgentSaga
// runs a workflow.Definition through the orchestrator saga (not workflow.Engine),
// the StepConfirm gate runs through e.confirmFn, and every StepTrace lands in
// the sink set via SetStepSink (stamped with the skill id + turn id).
func TestEngine_RunAgentSaga_DrivesSaga(t *testing.T) {
	exec := &sagaFakeExecutor{}
	eng := NewWithDeps(nil, exec, func(string, map[string]any) bool { return true })
	sink := &sagaFakeSink{}
	eng.SetStepSink(sink)

	def := &workflow.Definition{Name: "demo", Steps: []workflow.Step{
		{Name: "查询", Type: workflow.StepToolCall, Tool: "DescribeCompShareInstance",
			BuildArgs: func(*workflow.Context) (map[string]any, error) {
				return map[string]any{"UHostIds": []any{"u-1"}}, nil
			}},
		{Name: "确认", Type: workflow.StepConfirm, BuildArgs: func(*workflow.Context) (map[string]any, error) {
			return map[string]any{}, nil
		}},
	}}

	result, err := eng.RunAgentSaga(context.Background(), def, nil, "test_skill")
	require.NoError(t, err)
	assert.True(t, result.Success)
	assert.Equal(t, []string{"DescribeCompShareInstance"}, exec.calls)

	require.NotEmpty(t, sink.steps)
	assert.Equal(t, "test_skill", sink.steps[0].SkillID)
	assert.Equal(t, "turn-0", sink.steps[0].TurnID)
	assert.True(t, sink.hasState(observability.StepStateAwaitingConfirm), "confirm gate must emit awaiting_confirm")
	assert.True(t, sink.hasState(observability.StepStateSuccess))
}

// TestEngine_RunAgentSaga_NoDoubleConfirmOnL1 pins the key wiring property:
// because RunAgentSaga uses NewWithSafeExecutor (OriginWorkflowInternal), an L1
// mutating step run inside the saga does NOT trigger the SafeToolExecutor's own
// NeedsConfirm gate — only the saga's explicit StepConfirm does. confirmFn is
// therefore called exactly ONCE (for the StepConfirm), not twice. If the
// executor used OriginDirectLLM, the L1 tool step would re-prompt → 2 calls.
func TestEngine_RunAgentSaga_NoDoubleConfirmOnL1(t *testing.T) {
	exec := &sagaFakeExecutor{}
	var confirmCalls int
	eng := NewWithDeps(nil, exec, func(string, map[string]any) bool {
		confirmCalls++
		return true
	})
	eng.SetMutatingToolsEnabled(true) // allow the L1 step to run
	eng.SetStepSink(&sagaFakeSink{})

	def := &workflow.Definition{Name: "stop-demo", Steps: []workflow.Step{
		{Name: "确认", Type: workflow.StepConfirm, BuildArgs: func(*workflow.Context) (map[string]any, error) {
			return map[string]any{"UHostId": "u-1"}, nil
		}},
		{Name: "关机", Type: workflow.StepToolCall, Tool: "StopCompShareInstance", // L1 mutating
			BuildArgs: func(*workflow.Context) (map[string]any, error) {
				return map[string]any{"UHostId": "u-1"}, nil
			}},
	}}

	result, err := eng.RunAgentSaga(context.Background(), def, nil, "stop_skill")
	require.NoError(t, err)
	assert.True(t, result.Success)
	assert.Equal(t, []string{"StopCompShareInstance"}, exec.calls, "L1 step ran (OriginWorkflowInternal)")
	assert.Equal(t, 1, confirmCalls, "exactly one confirm (the StepConfirm); the L1 tool must NOT re-prompt")
}
