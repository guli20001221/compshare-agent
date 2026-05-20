package diagnosis

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestImageIssue_InstallFail(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":            "uhost-abc",
				"State":              "Install Fail",
				"CompShareImageName": "SD WebUI v1.9",
			},
		}},
	}}
	onStep, _ := collectEvents()

	chain := ImageIssueChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "初始化失败")
	assert.Contains(t, result.Conclusion, "SD WebUI")
	assert.Contains(t, result.Suggestion, "官方")
	assert.Len(t, executor.calls, 1)
}

func TestImageIssue_Installing(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{"UHostId": "uhost-abc", "State": "Install"},
		}},
	}}
	onStep, _ := collectEvents()

	chain := ImageIssueChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "初始化中")
	assert.Len(t, executor.calls, 1)
}

func TestImageIssue_Starting(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{"UHostId": "uhost-abc", "State": "Starting"},
		}},
	}}
	onStep, _ := collectEvents()

	chain := ImageIssueChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "启动中")
	assert.Contains(t, result.Conclusion, "不产生费用")
	assert.NotContains(t, result.Conclusion, "未运行", "Starting should not say '未运行'")
}

func TestImageIssue_Installing_Threshold(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{"UHostId": "uhost-abc", "State": "Install"},
		}},
	}}
	onStep, _ := collectEvents()

	chain := ImageIssueChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.Contains(t, result.Suggestion, "5 分钟", "should align with FAQ threshold")
	assert.NotContains(t, result.Suggestion, "10 分钟", "old threshold should be gone")
}

func TestImageIssue_CommunityImage(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":            "uhost-abc",
				"State":              "Running",
				"CompShareImageType": "Community",
				"CompShareImageName": "SD WebUI v1.9",
			},
		}},
	}}
	onStep, _ := collectEvents()

	chain := ImageIssueChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "社区镜像")
	assert.Contains(t, result.Conclusion, "SD WebUI")
	assert.Contains(t, result.Suggestion, "镜像作者")
	assert.Len(t, executor.calls, 1)
}

func TestImageIssue_OfficialImage_Normal(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":            "uhost-abc",
				"State":              "Running",
				"CompShareImageType": "System",
				"CompShareImageName": "Ubuntu 22.04 CUDA 12",
			},
		}},
	}}
	onStep, _ := collectEvents()

	chain := ImageIssueChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "云侧实例已运行")
	assert.Contains(t, result.Conclusion, "System")
	assert.NotContains(t, result.Conclusion, "镜像加载正常")
}

func TestImageIssue_NotRunning(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{"UHostId": "uhost-abc", "State": "Stopped"},
		}},
	}}
	onStep, _ := collectEvents()

	chain := ImageIssueChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "未运行")
	assert.Len(t, executor.calls, 1)
}

func TestImageIssue_InstanceNotFound(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}},
	}}
	onStep, _ := collectEvents()

	chain := ImageIssueChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-xxx"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "未找到")
}
