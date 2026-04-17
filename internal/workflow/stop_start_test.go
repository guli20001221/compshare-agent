package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// stopMockExecutor returns a mock with results for the StopInstance workflow.
func stopMockExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-xxx",
				"Name":       "my-gpu",
				"State":      "Running",
				"GpuType":    "4090",
				"GPU":        float64(1),
				"ChargeType": "Dynamic",
			},
		}},
		"StopCompShareInstance": {"RetCode": 0},
	}}
}

// stoppedMockExecutor returns a mock where the instance is already stopped.
func stoppedMockExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-xxx",
				"Name":       "my-gpu",
				"State":      "Stopped",
				"GpuType":    "4090",
				"GPU":        float64(1),
				"ChargeType": "Dynamic",
			},
		}},
	}}
}

// startMockExecutor returns a mock with results for the StartInstance workflow.
func startMockExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-yyy",
				"Name":       "start-me",
				"State":      "Stopped",
				"Zone":       "cn-bj2-04",
				"GpuType":    "A100",
				"GPU":        float64(2),
				"ChargeType": "Month",
			},
		}},
		"StartCompShareInstance": {"RetCode": 0},
	}}
}

func TestStopInstance_HappyPath(t *testing.T) {
	executor := stopMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := StopInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-xxx",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Len(t, result.Steps, 3)
	for i := range result.Steps {
		assert.Equal(t, def.Steps[i].Name, result.Steps[i].Name)
		assert.Equal(t, "success", result.Steps[i].Status)
	}

	assert.Len(t, executor.calls, 2)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
	assert.Equal(t, "StopCompShareInstance", executor.calls[1].action)
}

func TestStopInstance_ConfirmDenied(t *testing.T) {
	executor := stopMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return false }
	onStep, _ := collectEvents()

	def := StopInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-xxx",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, def.Steps[1].Name, result.StoppedAt)
	assert.Len(t, executor.calls, 1)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
}

func TestStopInstance_ConfirmHasFeeWarning(t *testing.T) {
	executor := stopMockExecutor()

	var capturedArgs map[string]any
	confirmFn := func(action string, args map[string]any) bool {
		capturedArgs = args
		return false
	}
	onStep, _ := collectEvents()

	def := StopInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	_, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-xxx",
	})

	assert.NoError(t, err)
	assert.NotNil(t, capturedArgs)
	warning, ok := capturedArgs["warning"].(string)
	assert.True(t, ok)
	assert.NotEmpty(t, warning)
}

func TestStopInstance_AlreadyStopped(t *testing.T) {
	executor := stoppedMockExecutor()
	onStep, _ := collectEvents()

	def := StopInstanceDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-xxx",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, def.Steps[0].Name, result.StoppedAt)
	assert.Len(t, executor.calls, 1)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
}

func TestStopInstance_NotFound(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}},
	}}
	onStep, _ := collectEvents()

	def := StopInstanceDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-nonexistent",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "查询实例", result.StoppedAt)
	assert.Contains(t, result.Message, "未找到")
	// StopCompShareInstance should NOT be called
	assert.Len(t, executor.calls, 1)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
}

func TestStartInstance_HappyPath(t *testing.T) {
	executor := startMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := StartInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-yyy",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Len(t, result.Steps, 3)
	for i := range result.Steps {
		assert.Equal(t, def.Steps[i].Name, result.Steps[i].Name)
		assert.Equal(t, "success", result.Steps[i].Status)
	}

	assert.Len(t, executor.calls, 2)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
	assert.Equal(t, "StartCompShareInstance", executor.calls[1].action)
	assert.Equal(t, "cn-bj2-04", executor.calls[1].args["Zone"])
}

func TestStartInstance_ConfirmDenied(t *testing.T) {
	executor := startMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return false }
	onStep, _ := collectEvents()

	def := StartInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-yyy",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, def.Steps[1].Name, result.StoppedAt)
	assert.Len(t, executor.calls, 1)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
}

func TestStartInstance_ConfirmShowsSummary(t *testing.T) {
	executor := startMockExecutor()

	var capturedArgs map[string]any
	confirmFn := func(action string, args map[string]any) bool {
		capturedArgs = args
		return false
	}
	onStep, _ := collectEvents()

	def := StartInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	_, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-yyy",
	})

	assert.NoError(t, err)
	assert.NotNil(t, capturedArgs)
	assert.Equal(t, "uhost-yyy", capturedArgs["UHostId"])
	assert.Equal(t, "start-me", capturedArgs["Name"])
	assert.Equal(t, "Stopped", capturedArgs["State"])
	assert.Equal(t, "A100", capturedArgs["GpuType"])
	assert.Equal(t, float64(2), capturedArgs["GPU"])
	assert.Equal(t, "Month", capturedArgs["ChargeType"])
}

func TestStartInstance_RunningRejected(t *testing.T) {
	executor := startMockExecutor()
	executor.results["DescribeCompShareInstance"] = map[string]any{
		"UHostSet": []any{
			map[string]any{
				"UHostId": "uhost-yyy",
				"State":   "Running",
			},
		},
	}
	onStep, _ := collectEvents()

	def := StartInstanceDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-yyy",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, def.Steps[0].Name, result.StoppedAt)
	assert.NotEmpty(t, result.Message)
	assert.Len(t, executor.calls, 1)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
}
