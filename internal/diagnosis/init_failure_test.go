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
					"State":              "Install Fail",
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
	assert.Equal(t, []any{"uhost-abc"}, executor.calls[0].args["UHostIds"])

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
	assert.Contains(t, result.Suggestion, "5 分钟")
	assert.Equal(t, "check_init_state", result.StoppedAt)
	assert.Len(t, result.Steps, 1)
	assert.Equal(t, "concluded", result.Steps[0].Status)
}

func TestInitFailureChain_Starting(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId": "uhost-start",
					"State":   "Starting",
				},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := InitFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{
		"UHostId": "uhost-start",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "启动中")
	assert.Contains(t, result.Conclusion, "不产生费用")
	assert.Contains(t, result.Suggestion, "1-2 分钟")
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

func TestInitFailureChain_ScanAll_MultipleFailures(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-1", "Name": "ok-gpu", "State": "Running"},
				map[string]any{"UHostId": "uhost-2", "Name": "fail-a", "State": "Install Fail", "CompShareImageName": "SD WebUI"},
				map[string]any{"UHostId": "uhost-3", "Name": "fail-b", "State": "Install Fail", "CompShareImageName": "ComfyUI"},
				map[string]any{"UHostId": "uhost-4", "Name": "init-c", "State": "Install"},
				map[string]any{"UHostId": "uhost-5", "Name": "boot-d", "State": "Starting"},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := InitFailureChain()
	eng := NewEngine(executor, onStep)
	// No UHostId → scan all
	result, err := eng.Run(context.Background(), chain, map[string]any{})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	// Should report both failures
	assert.Contains(t, result.Conclusion, "uhost-2")
	assert.Contains(t, result.Conclusion, "uhost-3")
	assert.Contains(t, result.Conclusion, "SD WebUI")
	assert.Contains(t, result.Conclusion, "ComfyUI")
	assert.Contains(t, result.Conclusion, "2 台")
	// Should also mention the installing one
	assert.Contains(t, result.Conclusion, "uhost-4")
	assert.Contains(t, result.Conclusion, "初始化中")
	// Should report the Starting one
	assert.Contains(t, result.Conclusion, "uhost-5")
	assert.Contains(t, result.Conclusion, "启动中")
	assert.Contains(t, result.Suggestion, "不收费")
	// Should NOT mention the running one as a problem
	assert.NotContains(t, result.Conclusion, "uhost-1")
	// Args should be empty (no UHostIds filter)
	assert.Empty(t, executor.calls[0].args["UHostIds"])
}

func TestInitFailureChain_ScanAll_NoneAbnormal(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-1", "Name": "gpu-a", "State": "Running"},
				map[string]any{"UHostId": "uhost-2", "Name": "gpu-b", "State": "Stopped"},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := InitFailureChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{})

	assert.NoError(t, err)
	assert.Contains(t, result.Conclusion, "所有实例状态正常")
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
