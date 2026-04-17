package workflow

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

func collectEvents() (func(StepEvent), *[]StepEvent) {
	var events []StepEvent
	return func(ev StepEvent) { events = append(events, ev) }, &events
}

// --- Tests ---

func TestContext_NewAndResult(t *testing.T) {
	params := map[string]any{"Zone": "cn-wlcb-a", "GpuType": "4090"}
	wfCtx := NewContext(params)

	assert.Equal(t, "cn-wlcb-a", wfCtx.Params["Zone"])
	assert.Equal(t, "4090", wfCtx.Params["GpuType"])
	assert.NotNil(t, wfCtx.StepResults)

	// Store and retrieve a step result
	wfCtx.StepResults["get_price"] = map[string]any{"Price": 3.5}
	result := wfCtx.Result("get_price")
	assert.Equal(t, 3.5, result["Price"])

	// Non-existent step returns nil
	assert.Nil(t, wfCtx.Result("nonexistent"))
}

func TestContext_NilParams(t *testing.T) {
	wfCtx := NewContext(nil)

	assert.NotNil(t, wfCtx.Params)
	assert.NotNil(t, wfCtx.StepResults)
	assert.Empty(t, wfCtx.Params)
	assert.Empty(t, wfCtx.StepResults)
}

func TestEngine_Run_Success(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"GetCompShareInstancePrice": {"Price": 3.5, "RetCode": 0},
		"CreateCompShareInstance":   {"UHostId": "uhost-abc", "RetCode": 0},
	}}
	onStep, events := collectEvents()

	def := &Definition{
		Name:        "CreateInstance",
		Description: "创建实例",
		Steps: []Step{
			{
				Name: "get_price",
				Type: StepToolCall,
				Tool: "GetCompShareInstancePrice",
				BuildArgs: func(wfCtx *Context) (map[string]any, error) {
					return map[string]any{"GpuType": wfCtx.Params["GpuType"]}, nil
				},
			},
			{
				Name: "create",
				Type: StepToolCall,
				Tool: "CreateCompShareInstance",
				BuildArgs: func(wfCtx *Context) (map[string]any, error) {
					price := wfCtx.Result("get_price")
					return map[string]any{
						"GpuType": wfCtx.Params["GpuType"],
						"Price":   price["Price"],
					}, nil
				},
			},
		},
	}

	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{"GpuType": "4090"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Equal(t, "工作流执行完成", result.Message)
	assert.Len(t, result.Steps, 2)
	assert.Equal(t, "success", result.Steps[0].Status)
	assert.Equal(t, "success", result.Steps[1].Status)
	assert.Empty(t, result.StoppedAt)

	// Both tools called
	assert.Len(t, executor.calls, 2)
	assert.Equal(t, "GetCompShareInstancePrice", executor.calls[0].action)
	assert.Equal(t, "CreateCompShareInstance", executor.calls[1].action)

	// Step2 args should include price from step1
	assert.Equal(t, 3.5, executor.calls[1].args["Price"])

	// Events emitted
	assert.NotEmpty(t, *events)
}

func TestEngine_Run_StepFailure(t *testing.T) {
	executor := &mockExecutor{failOn: "GetCompShareInstancePrice"}
	onStep, _ := collectEvents()

	def := &Definition{
		Name: "CreateInstance",
		Steps: []Step{
			{
				Name: "get_price",
				Type: StepToolCall,
				Tool: "GetCompShareInstancePrice",
				BuildArgs: func(wfCtx *Context) (map[string]any, error) {
					return map[string]any{}, nil
				},
			},
			{
				Name: "create",
				Type: StepToolCall,
				Tool: "CreateCompShareInstance",
				BuildArgs: func(wfCtx *Context) (map[string]any, error) {
					return map[string]any{}, nil
				},
			},
		},
	}

	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, nil)

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "get_price", result.StoppedAt)
	assert.Contains(t, result.Message, "get_price")
	assert.Len(t, result.Steps, 1)
	assert.Equal(t, "failed", result.Steps[0].Status)

	// Step2 should never have been called
	assert.Len(t, executor.calls, 1)
}

func TestEngine_Run_ConfirmApproved(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"StopCompShareInstance": {"RetCode": 0},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := &Definition{
		Name: "StopInstance",
		Steps: []Step{
			{
				Name: "confirm_stop",
				Type: StepConfirm,
				BuildArgs: func(wfCtx *Context) (map[string]any, error) {
					return map[string]any{"UHostId": wfCtx.Params["UHostId"]}, nil
				},
			},
			{
				Name: "stop",
				Type: StepToolCall,
				Tool: "StopCompShareInstance",
				BuildArgs: func(wfCtx *Context) (map[string]any, error) {
					return map[string]any{"UHostId": wfCtx.Params["UHostId"]}, nil
				},
			},
		},
	}

	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{"UHostId": "uhost-xxx"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Len(t, result.Steps, 2)
	assert.Equal(t, "success", result.Steps[0].Status)
	assert.Equal(t, "success", result.Steps[1].Status)

	// Tool was called after confirmation
	assert.Len(t, executor.calls, 1)
	assert.Equal(t, "StopCompShareInstance", executor.calls[0].action)
}

func TestEngine_Run_ConfirmDenied(t *testing.T) {
	executor := &mockExecutor{}
	confirmFn := func(action string, args map[string]any) bool { return false }
	onStep, _ := collectEvents()

	def := &Definition{
		Name: "StopInstance",
		Steps: []Step{
			{
				Name: "confirm_stop",
				Type: StepConfirm,
				BuildArgs: func(wfCtx *Context) (map[string]any, error) {
					return map[string]any{"UHostId": "uhost-xxx"}, nil
				},
			},
			{
				Name: "stop",
				Type: StepToolCall,
				Tool: "StopCompShareInstance",
				BuildArgs: func(wfCtx *Context) (map[string]any, error) {
					return map[string]any{"UHostId": "uhost-xxx"}, nil
				},
			},
		},
	}

	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, nil)

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "confirm_stop", result.StoppedAt)
	assert.Equal(t, "用户取消了操作", result.Message)
	assert.Len(t, result.Steps, 1)
	assert.Equal(t, "cancelled", result.Steps[0].Status)

	// Stop tool should never have been called
	assert.Empty(t, executor.calls)
}

func TestEngine_Run_NilConfirmFn(t *testing.T) {
	executor := &mockExecutor{}
	onStep, _ := collectEvents()

	def := &Definition{
		Name: "StopInstance",
		Steps: []Step{
			{
				Name: "confirm_stop",
				Type: StepConfirm,
				BuildArgs: func(wfCtx *Context) (map[string]any, error) {
					return map[string]any{}, nil
				},
			},
			{
				Name: "stop",
				Type: StepToolCall,
				Tool: "StopCompShareInstance",
				BuildArgs: func(wfCtx *Context) (map[string]any, error) {
					return map[string]any{}, nil
				},
			},
		},
	}

	// confirmFn is nil — should be treated as denial
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, nil)

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "confirm_stop", result.StoppedAt)
	assert.Equal(t, "用户取消了操作", result.Message)

	// Stop tool should never have been called
	assert.Empty(t, executor.calls)
}

func TestEngine_Run_CheckResultStops(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"GetCompShareInstancePrice": {"Price": 0, "RetCode": 0},
	}}
	onStep, _ := collectEvents()

	def := &Definition{
		Name: "CreateInstance",
		Steps: []Step{
			{
				Name: "get_price",
				Type: StepToolCall,
				Tool: "GetCompShareInstancePrice",
				BuildArgs: func(wfCtx *Context) (map[string]any, error) {
					return map[string]any{}, nil
				},
				CheckResult: func(_ *Context, result map[string]any) (bool, string) {
					if result["Price"] == 0 {
						return false, "价格为零，无法继续"
					}
					return true, ""
				},
			},
			{
				Name: "create",
				Type: StepToolCall,
				Tool: "CreateCompShareInstance",
				BuildArgs: func(wfCtx *Context) (map[string]any, error) {
					return map[string]any{}, nil
				},
			},
		},
	}

	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, nil)

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "get_price", result.StoppedAt)
	assert.Equal(t, "价格为零，无法继续", result.Message)
	assert.Len(t, result.Steps, 1)
	assert.Equal(t, "failed", result.Steps[0].Status)
	assert.Equal(t, "价格为零，无法继续", result.Steps[0].Message)

	// Only first tool called
	assert.Len(t, executor.calls, 1)
}

func TestEngine_Run_BuildArgsError(t *testing.T) {
	executor := &mockExecutor{}
	onStep, _ := collectEvents()

	def := &Definition{
		Name: "CreateInstance",
		Steps: []Step{
			{
				Name: "get_price",
				Type: StepToolCall,
				Tool: "GetCompShareInstancePrice",
				BuildArgs: func(wfCtx *Context) (map[string]any, error) {
					return nil, fmt.Errorf("missing required param: GpuType")
				},
			},
			{
				Name: "create",
				Type: StepToolCall,
				Tool: "CreateCompShareInstance",
				BuildArgs: func(wfCtx *Context) (map[string]any, error) {
					return map[string]any{}, nil
				},
			},
		},
	}

	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, nil)

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "get_price", result.StoppedAt)
	assert.Contains(t, result.Message, "参数构建失败")
	assert.Contains(t, result.Message, "missing required param: GpuType")
	assert.Len(t, result.Steps, 1)
	assert.Equal(t, "failed", result.Steps[0].Status)

	// Executor should never have been called
	assert.Empty(t, executor.calls)
}

func TestEngine_Run_ToolFuncOverridesTool(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DynamicTool": {"ok": true},
	}}
	onStep, events := collectEvents()

	def := &Definition{
		Name: "ToolFuncTest",
		Steps: []Step{
			{
				Name: "dynamic_step",
				Type: StepToolCall,
				Tool: "StaticTool", // should be ignored
				ToolFunc: func(wfCtx *Context) string {
					return "DynamicTool" // should override Tool
				},
				BuildArgs: func(wfCtx *Context) (map[string]any, error) {
					return map[string]any{"key": "value"}, nil
				},
			},
		},
	}

	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, nil)

	assert.NoError(t, err)
	assert.True(t, result.Success)

	// Executor should have been called with "DynamicTool", not "StaticTool"
	assert.Len(t, executor.calls, 1)
	assert.Equal(t, "DynamicTool", executor.calls[0].action)

	// Events should reference "DynamicTool"
	for _, ev := range *events {
		assert.Equal(t, "DynamicTool", ev.Tool)
	}
}

func TestEngine_Run_ToolFuncNil_UsesTool(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"StaticTool": {"ok": true},
	}}
	onStep, _ := collectEvents()

	def := &Definition{
		Name: "StaticTest",
		Steps: []Step{
			{
				Name: "static_step",
				Type: StepToolCall,
				Tool: "StaticTool",
				// ToolFunc is nil — should use Tool
				BuildArgs: func(wfCtx *Context) (map[string]any, error) {
					return map[string]any{}, nil
				},
			},
		},
	}

	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, nil)

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Len(t, executor.calls, 1)
	assert.Equal(t, "StaticTool", executor.calls[0].action)
}

func TestEngine_Run_EventIndexAndTotal(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"ToolA": {"ok": true},
		"ToolB": {"ok": true},
		"ToolC": {"ok": true},
	}}
	onStep, events := collectEvents()

	def := &Definition{
		Name: "ThreeSteps",
		Steps: []Step{
			{
				Name: "step_a",
				Type: StepToolCall,
				Tool: "ToolA",
				BuildArgs: func(wfCtx *Context) (map[string]any, error) {
					return map[string]any{}, nil
				},
			},
			{
				Name: "step_b",
				Type: StepToolCall,
				Tool: "ToolB",
				BuildArgs: func(wfCtx *Context) (map[string]any, error) {
					return map[string]any{}, nil
				},
			},
			{
				Name: "step_c",
				Type: StepToolCall,
				Tool: "ToolC",
				BuildArgs: func(wfCtx *Context) (map[string]any, error) {
					return map[string]any{}, nil
				},
			},
		},
	}

	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, nil)

	assert.NoError(t, err)
	assert.True(t, result.Success)

	// Each step emits 2 events: running + success => 6 total
	assert.Len(t, *events, 6)

	// All events should have Total == 3
	for _, ev := range *events {
		assert.Equal(t, 3, ev.Total)
	}

	// Check StepIndex values: running events for step_a=0, step_b=1, step_c=2
	runningEvents := make([]StepEvent, 0)
	for _, ev := range *events {
		if ev.Status == "running" {
			runningEvents = append(runningEvents, ev)
		}
	}
	assert.Len(t, runningEvents, 3)
	assert.Equal(t, 0, runningEvents[0].StepIndex)
	assert.Equal(t, "step_a", runningEvents[0].StepName)
	assert.Equal(t, 1, runningEvents[1].StepIndex)
	assert.Equal(t, "step_b", runningEvents[1].StepName)
	assert.Equal(t, 2, runningEvents[2].StepIndex)
	assert.Equal(t, "step_c", runningEvents[2].StepName)
}
