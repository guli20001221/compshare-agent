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
	assert.Equal(t, "工作流执行完成", result.Message)

	// All 3 steps completed
	assert.Len(t, result.Steps, 3)
	expectedNames := []string{"查询实例", "确认关机", "关机"}
	for i, name := range expectedNames {
		assert.Equal(t, name, result.Steps[i].Name)
		assert.Equal(t, "success", result.Steps[i].Status)
	}

	// 2 API calls (confirm does not call executor)
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
	assert.Equal(t, "确认关机", result.StoppedAt)
	assert.Equal(t, "用户取消了操作", result.Message)

	// Only DescribeCompShareInstance called; StopCompShareInstance never called
	assert.Len(t, executor.calls, 1)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
}

func TestStopInstance_ConfirmHasFeeWarning(t *testing.T) {
	executor := stopMockExecutor()

	var capturedArgs map[string]any
	confirmFn := func(action string, args map[string]any) bool {
		capturedArgs = args
		return false // deny to inspect args
	}
	onStep, _ := collectEvents()

	def := StopInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	_, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-xxx",
	})

	assert.NoError(t, err)
	assert.NotNil(t, capturedArgs)

	// Verify the warning about disk fees is present
	warning, ok := capturedArgs["warning"].(string)
	assert.True(t, ok)
	assert.Contains(t, warning, "磁盘")
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
	assert.Equal(t, "查询实例", result.StoppedAt)
	assert.Contains(t, result.Message, "已经是关机状态")

	// Only the describe call was made
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
	assert.Equal(t, "工作流执行完成", result.Message)

	// All 2 steps completed
	assert.Len(t, result.Steps, 2)
	expectedNames := []string{"确认开机", "开机"}
	for i, name := range expectedNames {
		assert.Equal(t, name, result.Steps[i].Name)
		assert.Equal(t, "success", result.Steps[i].Status)
	}

	// 1 API call (confirm does not call executor)
	assert.Len(t, executor.calls, 1)
	assert.Equal(t, "StartCompShareInstance", executor.calls[0].action)
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
	assert.Equal(t, "确认开机", result.StoppedAt)
	assert.Equal(t, "用户取消了操作", result.Message)

	// StartCompShareInstance never called
	assert.Empty(t, executor.calls)
}
