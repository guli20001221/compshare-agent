package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func runningMockExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-xxx",
				"Name":       "my-gpu",
				"State":      "Running",
				"Zone":       "cn-wlcb-01",
				"GpuType":    "4090",
				"GPU":        float64(1),
				"ChargeType": "Dynamic",
			},
		}},
		"RebootCompShareInstance": {"RetCode": float64(0)},
	}}
}

func TestRebootInstance_HappyPath(t *testing.T) {
	executor := runningMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := RebootInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-xxx",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Equal(t, "工作流执行完成", result.Message)

	assert.Len(t, result.Steps, 3)
	expectedNames := []string{"查询实例", "确认重启", "重启"}
	for i, name := range expectedNames {
		assert.Equal(t, name, result.Steps[i].Name)
		assert.Equal(t, "success", result.Steps[i].Status)
	}

	assert.Len(t, executor.calls, 2)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
	assert.Equal(t, "RebootCompShareInstance", executor.calls[1].action)
}

func TestRebootInstance_NotRunning(t *testing.T) {
	executor := stoppedMockExecutor()
	onStep, _ := collectEvents()

	def := RebootInstanceDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-xxx",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "查询实例", result.StoppedAt)
	assert.Contains(t, result.Message, "关机状态")
	assert.Len(t, executor.calls, 1)
}

func TestRebootInstance_ConfirmDenied(t *testing.T) {
	executor := runningMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return false }
	onStep, _ := collectEvents()

	def := RebootInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-xxx",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "确认重启", result.StoppedAt)
	assert.Equal(t, "用户取消了操作", result.Message)
	assert.Len(t, executor.calls, 1)
}

func TestRebootInstance_ConfirmHasWarning(t *testing.T) {
	executor := runningMockExecutor()

	var capturedArgs map[string]any
	confirmFn := func(action string, args map[string]any) bool {
		capturedArgs = args
		return false
	}
	onStep, _ := collectEvents()

	def := RebootInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	_, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-xxx",
	})

	assert.NoError(t, err)
	assert.NotNil(t, capturedArgs)

	warning, ok := capturedArgs["warning"].(string)
	assert.True(t, ok)
	assert.Contains(t, warning, "中断")
}

func TestRebootInstance_NotFound(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}},
	}}
	onStep, _ := collectEvents()

	def := RebootInstanceDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-nonexistent",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "查询实例", result.StoppedAt)
	assert.Contains(t, result.Message, "未找到")
	assert.Len(t, executor.calls, 1)
}
