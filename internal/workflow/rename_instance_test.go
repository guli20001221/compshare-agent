package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func renameMockExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-xxx",
				"Name":       "old-name",
				"State":      "Running",
				"Region":     "cn-wlcb",
				"Zone":       "cn-wlcb-01",
				"GpuType":    "4090",
				"GPU":        float64(1),
				"ChargeType": "Dynamic",
			},
		}},
		"ModifyCompShareInstanceName": {"UHostId": "uhost-xxx", "RetCode": float64(0)},
	}}
}

func TestRenameInstance_HappyPath(t *testing.T) {
	executor := renameMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := RenameInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-xxx",
		"Name":    "new-name",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Equal(t, "工作流执行完成", result.Message)

	assert.Len(t, result.Steps, 4)
	expectedNames := []string{"查询实例", "查询支持区", "确认改名", "修改名称"}
	for i, name := range expectedNames {
		assert.Equal(t, name, result.Steps[i].Name)
		assert.Equal(t, "success", result.Steps[i].Status)
	}

	assert.Len(t, executor.calls, 3)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
	assert.Equal(t, "DescribeCompShareSupportZone", executor.calls[1].action)
	assert.Equal(t, "ModifyCompShareInstanceName", executor.calls[2].action)
	assert.Equal(t, "new-name", executor.calls[2].args["Name"])
}

func TestRenameInstance_ConfirmShowsNewName(t *testing.T) {
	executor := renameMockExecutor()

	var capturedArgs map[string]any
	confirmFn := func(action string, args map[string]any) bool {
		capturedArgs = args
		return false
	}
	onStep, _ := collectEvents()

	def := RenameInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	_, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-xxx",
		"Name":    "new-name",
	})

	assert.NoError(t, err)
	assert.NotNil(t, capturedArgs)
	assert.Equal(t, "new-name", capturedArgs["NewName"])
	assert.Equal(t, "old-name", capturedArgs["Name"])
}

func TestRenameInstance_MissingNameAsksBeforeConfirmOrMutatingCall(t *testing.T) {
	executor := renameMockExecutor()
	confirmFn := func(action string, args map[string]any) bool {
		t.Fatalf("missing rename target must stop before confirmation; action=%s args=%v", action, args)
		return false
	}
	onStep, _ := collectEvents()

	def := RenameInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-xxx",
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "确认改名", result.StoppedAt)
	assert.Equal(t, []string{"name"}, result.MissingSlots)
	assert.Contains(t, result.Message, "改名需要指定新名称")
	assert.Len(t, executor.calls, 2)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
	assert.Equal(t, "DescribeCompShareSupportZone", executor.calls[1].action)
}

func TestRenameInstance_InstanceNotFound(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}},
	}}
	onStep, _ := collectEvents()

	def := RenameInstanceDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-zzz",
		"Name":    "new-name",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "查询实例", result.StoppedAt)
	assert.Contains(t, result.Message, "未找到")
}
