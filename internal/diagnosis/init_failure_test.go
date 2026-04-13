package diagnosis

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInitFailureChain_InstallFail(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId":            "uhost-abc",
					"State":              "InstallFail",
					"CompShareImageName": "PyTorch 2.1",
				},
			},
		},
	}}
	onStep, events := collectEvents()

	chain := InitFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{
		"UHostId": "uhost-abc",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "初始化失败")
	assert.Contains(t, result.Conclusion, "PyTorch 2.1")
	assert.Contains(t, result.Suggestion, "删除")
	assert.Equal(t, "check_init_state", result.StoppedAt)
	assert.Len(t, result.Steps, 1)
	assert.Equal(t, "concluded", result.Steps[0].Status)

	// Verify the tool was called with correct args
	assert.Len(t, executor.calls, 1)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
	assert.Equal(t, "uhost-abc", executor.calls[0].args["UHostId"])

	// Verify events were emitted (running + concluded)
	assert.GreaterOrEqual(t, len(*events), 2)
}

func TestInitFailureChain_Installing(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId": "uhost-def",
					"State":   "Install",
				},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := InitFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{
		"UHostId": "uhost-def",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "初始化中")
	assert.Contains(t, result.Suggestion, "10 分钟")
	assert.Equal(t, "check_init_state", result.StoppedAt)
	assert.Len(t, result.Steps, 1)
	assert.Equal(t, "concluded", result.Steps[0].Status)
}

func TestInitFailureChain_Running(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId": "uhost-ghi",
					"State":   "Running",
				},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := InitFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{
		"UHostId": "uhost-ghi",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "正常运行")
	assert.Contains(t, result.Suggestion, "初始化已成功完成")
	assert.Equal(t, "check_init_state", result.StoppedAt)
	assert.Len(t, result.Steps, 1)
	assert.Equal(t, "concluded", result.Steps[0].Status)
}

func TestInitFailureChain_NotFound(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{},
		},
	}}
	onStep, _ := collectEvents()

	chain := InitFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{
		"UHostId": "uhost-nonexist",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "未找到")
	assert.Equal(t, "check_init_state", result.StoppedAt)
	assert.Len(t, result.Steps, 1)
	assert.Equal(t, "concluded", result.Steps[0].Status)
}
