package orchestrator

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/workflow"
)

// --- fakes ---

type mockExecutor struct {
	results map[string]map[string]any
	errOn   map[string]error
	blockOn string // action that blocks until ctx is done (timeout test)
	calls   []string
}

func (m *mockExecutor) Execute(ctx context.Context, action string, _ map[string]any) (map[string]any, error) {
	m.calls = append(m.calls, action)
	if action == m.blockOn {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err, ok := m.errOn[action]; ok && err != nil {
		return nil, err
	}
	if r, ok := m.results[action]; ok {
		return r, nil
	}
	return map[string]any{"RetCode": float64(0)}, nil
}

func (m *mockExecutor) countOf(action string) int {
	n := 0
	for _, a := range m.calls {
		if a == action {
			n++
		}
	}
	return n
}

type captureSink struct{ steps []observability.StepTrace }

func (s *captureSink) EmitStep(st observability.StepTrace) error {
	s.steps = append(s.steps, st)
	return nil
}

// last returns the last StepTrace whose State matches, or false.
func (s *captureSink) lastWithState(state observability.StepState) (observability.StepTrace, bool) {
	for i := len(s.steps) - 1; i >= 0; i-- {
		if s.steps[i].State == state {
			return s.steps[i], true
		}
	}
	return observability.StepTrace{}, false
}

func (s *captureSink) states() []observability.StepState {
	out := make([]observability.StepState, len(s.steps))
	for i, st := range s.steps {
		out[i] = st.State
	}
	return out
}

// successArgs returns the Args recorded on the success StepTrace for the given
// tool (the args that were actually passed to the executor), or nil.
func (s *captureSink) successArgs(tool string) map[string]any {
	for _, st := range s.steps {
		if st.Tool == tool && st.State == observability.StepStateSuccess {
			return st.Args
		}
	}
	return nil
}

func approve(string, map[string]any) bool { return true }

// createInstanceFixtures returns faked API results sufficient for the real
// 7-step CreateInstanceDef to drive end-to-end (V100S × 1 → 8C/32GB). The
// GB→MB conversion in resolveTargetSpec (create_instance.go:142) means the
// create step receives Memory=32768 (MB), which the EndToEnd test asserts.
func createInstanceFixtures() map[string]map[string]any {
	return map[string]map[string]any{
		"DescribeCompShareImages": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-1", "Name": "Ubuntu22"},
		}},
		"DescribeAvailableCompShareInstanceTypes": {"AvailableInstanceTypes": []any{
			map[string]any{"Name": "V100S", "MachineSizes": []any{
				map[string]any{"Gpu": float64(1), "Collection": []any{
					map[string]any{"Cpu": float64(8), "Memory": []any{float64(32)}},
				}},
			}},
		}},
		"CheckCompShareResourceCapacity": {"Specs": []any{
			map[string]any{"Gpu": float64(1), "Cpu": float64(8), "Mem": float64(32), "ResourceEnough": true},
		}},
		"GetCompShareInstanceUserPrice": {"Price": float64(1.23)},
		"CreateCompShareInstance":       {"UHostIds": []any{"uhost-abc"}},
		"DescribeCompShareInstance":     {"UHostSet": []any{map[string]any{"State": "Running"}}},
	}
}

func toolStep(name, tool string) workflow.Step {
	return workflow.Step{
		Name: name,
		Type: workflow.StepToolCall,
		Tool: tool,
		BuildArgs: func(*workflow.Context) (map[string]any, error) {
			return map[string]any{"k": "v"}, nil
		},
	}
}

// --- tests ---

func TestSaga_Run_ForwardSuccess(t *testing.T) {
	exec := &mockExecutor{}
	sink := &captureSink{}
	s := New(Options{
		Executor:  exec,
		Confirm:   approve,
		Sink:      sink,
		SessionID: "sess-1",
		TurnID:    "turn-7",
		SkillID:   "deploy_demo",
	})

	def := &workflow.Definition{
		Name: "demo",
		Steps: []workflow.Step{
			toolStep("a", "DescribeCompShareInstance"),
			{Name: "confirm", Type: workflow.StepConfirm, BuildArgs: func(*workflow.Context) (map[string]any, error) {
				return map[string]any{"ok": true}, nil
			}},
			toolStep("b", "DescribeCompShareImages"),
		},
	}

	result, err := s.Run(context.Background(), def, nil)
	require.NoError(t, err)
	assert.True(t, result.Success)
	require.Len(t, result.Steps, 3)
	for _, st := range result.Steps {
		assert.Equal(t, "success", st.Status)
	}

	// running→success, awaiting_confirm→success, running→success
	assert.Equal(t, []observability.StepState{
		observability.StepStateRunning, observability.StepStateSuccess,
		observability.StepStateAwaitingConfirm, observability.StepStateSuccess,
		observability.StepStateRunning, observability.StepStateSuccess,
	}, sink.states())

	// identity stamped on every trace
	for _, st := range sink.steps {
		assert.Equal(t, "sess-1", st.SessionID)
		assert.Equal(t, "turn-7", st.TurnID)
		assert.Equal(t, "turn-7-saga", st.SagaID)
		assert.Equal(t, "deploy_demo", st.SkillID)
		assert.NotEmpty(t, st.StepID)
	}
	// deterministic per-step ids
	assert.Equal(t, "turn-7-saga-step-00", sink.steps[0].StepID)
	assert.Equal(t, "turn-7-saga-step-01", sink.steps[2].StepID) // confirm step
	assert.Equal(t, "turn-7-saga-step-02", sink.steps[4].StepID)
}

func TestSaga_Run_ToolError_StopsNoRetry(t *testing.T) {
	exec := &mockExecutor{errOn: map[string]error{"DescribeCompShareImages": fmt.Errorf("boom")}}
	sink := &captureSink{}
	s := New(Options{Executor: exec, Sink: sink, TurnID: "t1"})

	def := &workflow.Definition{Name: "demo", Steps: []workflow.Step{
		toolStep("a", "DescribeCompShareInstance"),
		toolStep("b", "DescribeCompShareImages"),
		toolStep("c", "DescribeCompShareInstance"),
	}}

	result, err := s.Run(context.Background(), def, nil)
	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "b", result.StoppedAt)
	assert.Len(t, result.Steps, 2) // a success + b failed; c never reached

	// no auto-retry: the failing action was executed exactly once
	assert.Equal(t, 1, exec.countOf("DescribeCompShareImages"))
	// step "a" ran once; step "c" (same tool) is never reached after the stop
	assert.Equal(t, 1, exec.countOf("DescribeCompShareInstance"))

	failed, ok := sink.lastWithState(observability.StepStateFailed)
	require.True(t, ok)
	assert.Equal(t, "api_error", failed.ErrorCategory)
}

func TestSaga_Run_ConfirmDenied_Cancelled(t *testing.T) {
	exec := &mockExecutor{}
	sink := &captureSink{}
	deny := func(string, map[string]any) bool { return false }
	s := New(Options{Executor: exec, Confirm: deny, Sink: sink, TurnID: "t1"})

	def := &workflow.Definition{Name: "demo", Steps: []workflow.Step{
		{Name: "confirm", Type: workflow.StepConfirm, BuildArgs: func(*workflow.Context) (map[string]any, error) {
			return map[string]any{}, nil
		}},
		toolStep("after", "CreateCompShareInstance"),
	}}

	result, err := s.Run(context.Background(), def, nil)
	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "confirm", result.StoppedAt)
	require.Len(t, result.Steps, 1)
	assert.Equal(t, "cancelled", result.Steps[0].Status)
	assert.Empty(t, exec.calls, "mutating step must not run after declined confirm")

	failed, ok := sink.lastWithState(observability.StepStateFailed)
	require.True(t, ok)
	assert.Equal(t, "user_abort", failed.ErrorCategory)
}

func TestSaga_Run_NilConfirm_Declines(t *testing.T) {
	exec := &mockExecutor{}
	s := New(Options{Executor: exec, Confirm: nil, TurnID: "t1"})
	def := &workflow.Definition{Name: "demo", Steps: []workflow.Step{
		{Name: "confirm", Type: workflow.StepConfirm, BuildArgs: func(*workflow.Context) (map[string]any, error) {
			return map[string]any{}, nil
		}},
		toolStep("after", "CreateCompShareInstance"),
	}}
	result, err := s.Run(context.Background(), def, nil)
	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "confirm", result.StoppedAt)
	assert.Empty(t, exec.calls)
}

func TestSaga_Run_Timeout(t *testing.T) {
	exec := &mockExecutor{blockOn: "DescribeCompShareInstance"}
	sink := &captureSink{}
	s := New(Options{Executor: exec, Sink: sink, TurnID: "t1"})

	slow := toolStep("slow", "DescribeCompShareInstance")
	slow.Timeout = 20 * time.Millisecond
	def := &workflow.Definition{Name: "demo", Steps: []workflow.Step{slow}}

	start := time.Now()
	result, err := s.Run(context.Background(), def, nil)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 2*time.Second, "step timeout must cut the blocking call short")
	assert.False(t, result.Success)
	assert.Equal(t, "slow", result.StoppedAt)

	timeout, ok := sink.lastWithState(observability.StepStateTimeout)
	require.True(t, ok, "a step timeout must emit StepStateTimeout, not failed")
	assert.Equal(t, "timeout", timeout.ErrorCategory)
}

func TestSaga_Run_CheckResultFalse(t *testing.T) {
	exec := &mockExecutor{}
	sink := &captureSink{}
	s := New(Options{Executor: exec, Sink: sink, TurnID: "t1"})

	step := toolStep("check", "DescribeCompShareInstance")
	step.CheckResult = func(*workflow.Context, map[string]any) (bool, string) {
		return false, "库存不足"
	}
	def := &workflow.Definition{Name: "demo", Steps: []workflow.Step{step}}

	result, err := s.Run(context.Background(), def, nil)
	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "库存不足", result.Message)

	failed, ok := sink.lastWithState(observability.StepStateFailed)
	require.True(t, ok)
	assert.Equal(t, "check_failed", failed.ErrorCategory)
}

func TestSaga_Run_BuildArgsError(t *testing.T) {
	exec := &mockExecutor{}
	s := New(Options{Executor: exec, TurnID: "t1"})
	def := &workflow.Definition{Name: "demo", Steps: []workflow.Step{{
		Name: "bad", Type: workflow.StepToolCall, Tool: "DescribeCompShareInstance",
		BuildArgs: func(*workflow.Context) (map[string]any, error) {
			return nil, fmt.Errorf("missing GpuType")
		},
	}}}
	result, err := s.Run(context.Background(), def, nil)
	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "参数构建失败")
	assert.Empty(t, exec.calls, "executor must not run when BuildArgs fails")
}

func TestSaga_Run_RejectsL2ForwardStep(t *testing.T) {
	exec := &mockExecutor{}
	s := New(Options{Executor: exec, TurnID: "t1"})
	def := &workflow.Definition{Name: "demo", Steps: []workflow.Step{
		toolStep("kill", "TerminateCompShareInstance"),
	}}
	_, err := s.Run(context.Background(), def, nil)
	require.Error(t, err, "saga must refuse an L2/destructive forward step")
	assert.Contains(t, err.Error(), "L2")
	assert.Empty(t, exec.calls, "no step runs when the definition is rejected")
}

func TestSaga_Run_RejectsL2Compensate(t *testing.T) {
	exec := &mockExecutor{}
	s := New(Options{Executor: exec, TurnID: "t1"})
	step := toolStep("create", "CreateCompShareInstance")
	step.Compensate = &workflow.CompensateStep{
		Tool: "TerminateCompShareInstance",
		BuildArgs: func(*workflow.Context, map[string]any) (map[string]any, error) {
			return nil, nil
		},
	}
	def := &workflow.Definition{Name: "demo", Steps: []workflow.Step{step}}
	_, err := s.Run(context.Background(), def, nil)
	require.Error(t, err, "saga must refuse a definition whose compensate is L2 (no auto-terminate; ADR-006 §决策2 Amendment)")
	assert.Contains(t, err.Error(), "compensate")
	assert.Empty(t, exec.calls)
}

func TestSaga_Run_RejectsL2ResolvedToolFunc(t *testing.T) {
	exec := &mockExecutor{}
	sink := &captureSink{}
	s := New(Options{Executor: exec, Sink: sink, TurnID: "t1"})
	def := &workflow.Definition{Name: "demo", Steps: []workflow.Step{{
		Name: "dyn", Type: workflow.StepToolCall,
		ToolFunc:  func(*workflow.Context) string { return "TerminateCompShareInstance" },
		BuildArgs: func(*workflow.Context) (map[string]any, error) { return map[string]any{}, nil },
	}}}
	result, err := s.Run(context.Background(), def, nil)
	require.NoError(t, err) // a step-level refusal is a Result failure, not a Go error
	assert.False(t, result.Success)
	assert.Empty(t, exec.calls, "an L2 action resolved at runtime must never reach the executor")
	failed, ok := sink.lastWithState(observability.StepStateFailed)
	require.True(t, ok)
	assert.Equal(t, "destructive_refused", failed.ErrorCategory)
}

func TestSaga_Run_NilSink_NoPanic(t *testing.T) {
	exec := &mockExecutor{}
	s := New(Options{Executor: exec, Sink: nil, Confirm: approve, TurnID: "t1"})
	def := &workflow.Definition{Name: "demo", Steps: []workflow.Step{toolStep("a", "DescribeCompShareInstance")}}
	result, err := s.Run(context.Background(), def, nil)
	require.NoError(t, err)
	assert.True(t, result.Success)
}

func TestSaga_Run_NilDefinition(t *testing.T) {
	s := New(Options{Executor: &mockExecutor{}})
	_, err := s.Run(context.Background(), nil, nil)
	require.Error(t, err)
}

func TestSaga_Run_ParentCtxCancelled(t *testing.T) {
	exec := &mockExecutor{}
	s := New(Options{Executor: exec, TurnID: "t1"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	def := &workflow.Definition{Name: "demo", Steps: []workflow.Step{toolStep("a", "DescribeCompShareInstance")}}
	result, err := s.Run(ctx, def, nil)
	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "已取消")
	assert.Empty(t, exec.calls)
}

func TestSaga_EffectiveTimeout(t *testing.T) {
	var warnings []string
	s := New(Options{
		Executor: &mockExecutor{},
		Logf:     func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) },
	})

	// zero → default (no warning)
	assert.Equal(t, DefaultStepTimeout, s.effectiveTimeout(workflow.Step{}))
	assert.Empty(t, warnings)

	// within default → used, no warning
	assert.Equal(t, 30*time.Second, s.effectiveTimeout(workflow.Step{Timeout: 30 * time.Second}))
	assert.Empty(t, warnings)

	// wider than 4 min → used but surfaced (widening isn't silent)
	wide := 10 * time.Minute
	assert.Equal(t, wide, s.effectiveTimeout(workflow.Step{Name: "poll", Timeout: wide}))
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "exceeds default")
}

// TestSaga_CreateInstanceDef_NoCompensate_NoL2 pins the ADR-006 §决策2
// Amendment: the create demo workflow carries NO Compensate on any step
// (instance creation is never auto-rolled-back) and contains no L2/destructive
// forward step, so the saga accepts it without error.
func TestSaga_CreateInstanceDef_NoCompensate_NoL2(t *testing.T) {
	def := workflow.CreateInstanceDef()
	for _, step := range def.Steps {
		assert.Nil(t, step.Compensate, "step %q must carry no Compensate (no auto-rollback of created instance)", step.Name)
	}
	assert.NoError(t, validateNoDestructive(def), "CreateInstanceDef must contain no L2 step")
}

// TestSaga_CreateInstanceDef_EndToEnd drives the real 7-step create workflow
// through the orchestrator with faked API results + an approving confirm,
// proving the demo runs end-to-end and emits a full StepTrace sequence
// including awaiting_confirm at the confirm gate. (The genuine live run against
// a real V100S is the B6.6 binary smoke.)
func TestSaga_CreateInstanceDef_EndToEnd(t *testing.T) {
	exec := &mockExecutor{results: createInstanceFixtures()}
	sink := &captureSink{}
	s := New(Options{
		Executor:  exec,
		Confirm:   approve,
		Sink:      sink,
		SessionID: "sess",
		TurnID:    "turn-1",
		SkillID:   "CreateInstanceWorkflow",
	})

	result, err := s.Run(context.Background(), workflow.CreateInstanceDef(), map[string]any{
		"GpuType": "V100S",
		"Gpu":     float64(1),
	})
	require.NoError(t, err)
	assert.True(t, result.Success, "result: %+v", result)
	require.Len(t, result.Steps, 7)
	for _, st := range result.Steps {
		assert.Equal(t, "success", st.Status, "step %q", st.Name)
	}

	// 6 tool steps (running+success) + 1 confirm (awaiting_confirm+success) = 14
	assert.Len(t, sink.steps, 14)
	_, hasConfirm := sink.lastWithState(observability.StepStateAwaitingConfirm)
	assert.True(t, hasConfirm, "confirm gate must emit awaiting_confirm")
	// the create result id flows to the status-poll step (verifies wfCtx threading)
	assert.Equal(t, 1, exec.countOf("CreateCompShareInstance"))
	assert.Equal(t, 1, exec.countOf("DescribeCompShareInstance"))

	// Falsifiable against resolveTargetSpec / BuildArgs regressions: the create
	// step must receive the GB→MB-converted Memory (32GB → 32768MB), the
	// resolved CPU, and the threaded image id — not vacuously pass.
	createArgs := sink.successArgs("CreateCompShareInstance")
	require.NotNil(t, createArgs)
	assert.Equal(t, float64(8), createArgs["CPU"])
	assert.Equal(t, float64(32768), createArgs["Memory"], "Memory must be MB (32GB×1024), not GB")
	assert.Equal(t, "img-1", createArgs["CompShareImageId"])
}

func TestSaga_StepResultThreadsThroughContext(t *testing.T) {
	exec := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostIds": []any{"u-1"}},
	}}
	s := New(Options{Executor: exec, TurnID: "t1"})
	var sawPrev map[string]any
	def := &workflow.Definition{Name: "demo", Steps: []workflow.Step{
		toolStep("first", "DescribeCompShareInstance"),
		{Name: "second", Type: workflow.StepToolCall, Tool: "DescribeCompShareImages",
			BuildArgs: func(wfCtx *workflow.Context) (map[string]any, error) {
				sawPrev = wfCtx.Result("first")
				return map[string]any{}, nil
			}},
	}}
	result, err := s.Run(context.Background(), def, nil)
	require.NoError(t, err)
	assert.True(t, result.Success)
	require.NotNil(t, sawPrev)
	assert.Equal(t, []any{"u-1"}, sawPrev["UHostIds"])
}

// TestSaga_CreateInstanceFail_StopsNoRetry pins ADR-006 §决策2 Amendment
// acceptance ① literally on its subject: when CreateCompShareInstance itself
// fails, the saga stops at the create step, does NOT retry, returns to the
// caller (= back to ConfirmFunc), and never reaches the status-poll step. There
// is nothing to roll back (create-fail → no instance → no billing).
func TestSaga_CreateInstanceFail_StopsNoRetry(t *testing.T) {
	exec := &mockExecutor{
		results: createInstanceFixtures(),
		errOn:   map[string]error{"CreateCompShareInstance": fmt.Errorf("RetCode=230 容量不足")},
	}
	s := New(Options{Executor: exec, Confirm: approve, TurnID: "t1", SkillID: "CreateInstanceWorkflow"})

	result, err := s.Run(context.Background(), workflow.CreateInstanceDef(), map[string]any{
		"GpuType": "V100S",
		"Gpu":     float64(1),
	})
	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "创建实例", result.StoppedAt)
	assert.Equal(t, 1, exec.countOf("CreateCompShareInstance"), "create must not auto-retry")
	assert.Equal(t, 0, exec.countOf("DescribeCompShareInstance"), "status-poll step must not run after create fails")
}

// TestSaga_Run_ParentCtxCancelledMidStep covers the mid-run cancellation path
// (parent ctx cancelled while the executor is blocking inside a step), distinct
// from the pre-cancel case. Documented behavior (step.go:82-86): a parent
// cancellation surfaces as a FAILED step (api_error), NOT a StepStateTimeout —
// step-timeout is reserved for the step's OWN deadline firing. This guards
// against a future "fix" that misreports parent cancellation as a timeout.
func TestSaga_Run_ParentCtxCancelledMidStep(t *testing.T) {
	exec := &mockExecutor{blockOn: "DescribeCompShareInstance"}
	sink := &captureSink{}
	s := New(Options{Executor: exec, Sink: sink, TurnID: "t1"})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	def := &workflow.Definition{Name: "demo", Steps: []workflow.Step{toolStep("a", "DescribeCompShareInstance")}}

	result, err := s.Run(ctx, def, nil)
	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "a", result.StoppedAt)
	assert.Equal(t, 1, exec.countOf("DescribeCompShareInstance"), "executor WAS entered (distinguishes from pre-cancel)")

	failed, ok := sink.lastWithState(observability.StepStateFailed)
	require.True(t, ok)
	assert.Equal(t, "api_error", failed.ErrorCategory)
	_, isTimeout := sink.lastWithState(observability.StepStateTimeout)
	assert.False(t, isTimeout, "parent cancellation must not be misreported as a step timeout")
}
