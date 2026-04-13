package diagnosis

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// --- Mock Executor ---

type mockExecutor struct {
	results map[string]map[string]any
	calls   []executorCall
	failOn  string
}

type executorCall struct {
	action string
	args   map[string]any
}

func (m *mockExecutor) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	m.calls = append(m.calls, executorCall{action, args})
	if action == m.failOn {
		return nil, fmt.Errorf("API error: %s failed", action)
	}
	if r, ok := m.results[action]; ok {
		return r, nil
	}
	return map[string]any{"RetCode": 0}, nil
}

// --- Helpers ---

func collectEvents() (func(DiagEvent), *[]DiagEvent) {
	var events []DiagEvent
	return func(ev DiagEvent) { events = append(events, ev) }, &events
}

// --- Tests ---

func TestContext_NewAndResult(t *testing.T) {
	params := map[string]any{"UHostId": "uhost-abc", "Zone": "cn-wlcb-a"}
	dCtx := NewContext(params)

	assert.Equal(t, "uhost-abc", dCtx.Params["UHostId"])
	assert.Equal(t, "cn-wlcb-a", dCtx.Params["Zone"])
	assert.NotNil(t, dCtx.StepResults)

	// Store and retrieve a step result
	dCtx.StepResults["check_state"] = map[string]any{"State": "Running"}
	result := dCtx.Result("check_state")
	assert.Equal(t, "Running", result["State"])

	// Non-existent step returns nil
	assert.Nil(t, dCtx.Result("nonexistent"))
}

func TestContext_NilParams(t *testing.T) {
	dCtx := NewContext(nil)

	assert.NotNil(t, dCtx.Params)
	assert.NotNil(t, dCtx.StepResults)
	assert.Empty(t, dCtx.Params)
	assert.Empty(t, dCtx.StepResults)
}

func TestEngine_Run_ConcludeAtFirstStep(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"State": "Stopped"},
	}}
	onStep, _ := collectEvents()

	chain := &Chain{
		Name: "DiagnoseTest",
		Steps: []Step{
			{
				Name: "check_state",
				Tool: "DescribeCompShareInstance",
				BuildArgs: func(dCtx *Context) (map[string]any, error) {
					return map[string]any{"UHostId": dCtx.Params["UHostId"]}, nil
				},
				Evaluate: func(result map[string]any, dCtx *Context) Verdict {
					if result["State"] == "Stopped" {
						return Verdict{Action: Conclude, Conclusion: "实例已关机", Suggestion: "请先开机"}
					}
					return Verdict{Action: Continue}
				},
			},
			{
				Name: "check_firewall",
				Tool: "DescribeFirewall",
				BuildArgs: func(dCtx *Context) (map[string]any, error) {
					return map[string]any{}, nil
				},
				Evaluate: func(result map[string]any, dCtx *Context) Verdict {
					return Verdict{Action: Continue}
				},
			},
		},
		Fallback: Verdict{Action: Conclude, Conclusion: "未发现问题"},
	}

	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Equal(t, "实例已关机", result.Conclusion)
	assert.Equal(t, "请先开机", result.Suggestion)
	assert.Equal(t, "check_state", result.StoppedAt)
	assert.Len(t, result.Steps, 1)
	assert.Equal(t, "concluded", result.Steps[0].Status)

	// Second step should never have been called
	assert.Len(t, executor.calls, 1)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
}

func TestEngine_Run_AllStepsPass_Fallback(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"State": "Running"},
		"DescribeFirewall":         {"Port22": "open"},
	}}
	onStep, _ := collectEvents()

	chain := &Chain{
		Name: "DiagnoseTest",
		Steps: []Step{
			{
				Name: "check_state",
				Tool: "DescribeCompShareInstance",
				BuildArgs: func(dCtx *Context) (map[string]any, error) {
					return map[string]any{"UHostId": dCtx.Params["UHostId"]}, nil
				},
				Evaluate: func(result map[string]any, dCtx *Context) Verdict {
					return Verdict{Action: Continue}
				},
			},
			{
				Name: "check_firewall",
				Tool: "DescribeFirewall",
				BuildArgs: func(dCtx *Context) (map[string]any, error) {
					return map[string]any{"UHostId": dCtx.Params["UHostId"]}, nil
				},
				Evaluate: func(result map[string]any, dCtx *Context) Verdict {
					return Verdict{Action: Continue}
				},
			},
		},
		Fallback: Verdict{
			Action:     Conclude,
			Conclusion: "所有检查均正常",
			Suggestion: "请检查客户端配置",
		},
	}

	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Equal(t, "所有检查均正常", result.Conclusion)
	assert.Equal(t, "请检查客户端配置", result.Suggestion)
	assert.Empty(t, result.StoppedAt)
	assert.Len(t, result.Steps, 2)
	assert.Equal(t, "checked", result.Steps[0].Status)
	assert.Equal(t, "checked", result.Steps[1].Status)

	// Both tools called
	assert.Len(t, executor.calls, 2)
}

func TestEngine_Run_ToolFailure(t *testing.T) {
	executor := &mockExecutor{failOn: "DescribeCompShareInstance"}
	onStep, _ := collectEvents()

	chain := &Chain{
		Name: "DiagnoseTest",
		Steps: []Step{
			{
				Name: "check_state",
				Tool: "DescribeCompShareInstance",
				BuildArgs: func(dCtx *Context) (map[string]any, error) {
					return map[string]any{}, nil
				},
				Evaluate: func(result map[string]any, dCtx *Context) Verdict {
					return Verdict{Action: Continue}
				},
			},
		},
		Fallback: Verdict{Action: Conclude, Conclusion: "未发现问题"},
	}

	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, nil)

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Conclusion, "检查失败")
	assert.Equal(t, "check_state", result.StoppedAt)
	assert.Len(t, result.Steps, 1)
	assert.Equal(t, "failed", result.Steps[0].Status)
}

func TestEngine_Run_BuildArgsError(t *testing.T) {
	executor := &mockExecutor{}
	onStep, _ := collectEvents()

	chain := &Chain{
		Name: "DiagnoseTest",
		Steps: []Step{
			{
				Name: "check_state",
				Tool: "DescribeCompShareInstance",
				BuildArgs: func(dCtx *Context) (map[string]any, error) {
					return nil, fmt.Errorf("missing required param: UHostId")
				},
				Evaluate: func(result map[string]any, dCtx *Context) Verdict {
					return Verdict{Action: Continue}
				},
			},
		},
		Fallback: Verdict{Action: Conclude, Conclusion: "未发现问题"},
	}

	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, nil)

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Conclusion, "参数构建失败")
	assert.Equal(t, "check_state", result.StoppedAt)
	assert.Len(t, result.Steps, 1)
	assert.Equal(t, "failed", result.Steps[0].Status)

	// Executor should never have been called
	assert.Empty(t, executor.calls)
}

func TestEngine_Run_ContextCancelled(t *testing.T) {
	executor := &mockExecutor{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	chain := &Chain{
		Name: "DiagnoseTest",
		Steps: []Step{
			{
				Name: "check_state",
				Tool: "DescribeCompShareInstance",
				BuildArgs: func(dCtx *Context) (map[string]any, error) {
					return map[string]any{}, nil
				},
				Evaluate: func(result map[string]any, dCtx *Context) Verdict {
					return Verdict{Action: Continue}
				},
			},
		},
		Fallback: Verdict{Action: Conclude, Conclusion: "未发现问题"},
	}

	eng := NewEngine(executor, nil)
	result, err := eng.Run(ctx, chain, nil)

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Conclusion, "已取消")

	// Executor should never have been called
	assert.Empty(t, executor.calls)
}

func TestEngine_Run_StepResultsAccumulate(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"ToolA": {"ValueA": "hello"},
		"ToolB": {"ValueB": "world"},
	}}
	onStep, _ := collectEvents()

	var capturedFromStepB string

	chain := &Chain{
		Name: "DiagnoseAccumulate",
		Steps: []Step{
			{
				Name: "step_a",
				Tool: "ToolA",
				BuildArgs: func(dCtx *Context) (map[string]any, error) {
					return map[string]any{}, nil
				},
				Evaluate: func(result map[string]any, dCtx *Context) Verdict {
					return Verdict{Action: Continue}
				},
			},
			{
				Name: "step_b",
				Tool: "ToolB",
				BuildArgs: func(dCtx *Context) (map[string]any, error) {
					// Access step_a's result during BuildArgs
					prev := dCtx.Result("step_a")
					if prev != nil {
						capturedFromStepB = prev["ValueA"].(string)
					}
					return map[string]any{}, nil
				},
				Evaluate: func(result map[string]any, dCtx *Context) Verdict {
					// Also verify step_a result is accessible during Evaluate
					prev := dCtx.Result("step_a")
					if prev != nil && prev["ValueA"] == "hello" {
						return Verdict{Action: Conclude, Conclusion: "accumulated OK"}
					}
					return Verdict{Action: Continue}
				},
			},
		},
		Fallback: Verdict{Action: Conclude, Conclusion: "fallback"},
	}

	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, nil)

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Equal(t, "hello", capturedFromStepB)
	assert.Equal(t, "accumulated OK", result.Conclusion)
	assert.Len(t, executor.calls, 2)
}

func TestRegistry_IsDiagnosisTool(t *testing.T) {
	assert.True(t, IsDiagnosisTool("DiagnoseSSH"))
	assert.True(t, IsDiagnosisTool("DiagnoseInitFailure"))
	assert.False(t, IsDiagnosisTool("DescribeCompShareInstance"))
	assert.False(t, IsDiagnosisTool("NonExistent"))
	assert.False(t, IsDiagnosisTool(""))
}

func TestRegistry_GetChain(t *testing.T) {
	chain, ok := GetChain("DiagnoseSSH")
	assert.True(t, ok)
	assert.NotNil(t, chain)
	assert.Equal(t, "DiagnoseSSH", chain.Name)

	chain2, ok2 := GetChain("DiagnoseInitFailure")
	assert.True(t, ok2)
	assert.NotNil(t, chain2)
	assert.Equal(t, "DiagnoseInitFailure", chain2.Name)

	chain3, ok3 := GetChain("NonExistent")
	assert.False(t, ok3)
	assert.Nil(t, chain3)
}
