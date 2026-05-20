package diagnosis

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGPUChain_Stopped(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-abc", "State": "Stopped"},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := GPUNotDetectedChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "关机")
	assert.Contains(t, result.Suggestion, "开机")
	assert.Equal(t, "检查实例状态与 GPU 配置", result.StoppedAt)
	assert.Len(t, executor.calls, 1)
}

func TestGPUChain_NameMatchesPublicTool(t *testing.T) {
	assert.Equal(t, "DiagnoseGPU", GPUNotDetectedChain().Name)
}

func TestGPUChain_Running_NoGPU(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId": "uhost-abc",
					"State":   "Running",
					"GPU":     float64(0),
					"GpuType": "",
				},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := GPUNotDetectedChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "无卡模式")
	assert.Contains(t, result.Suggestion, "带 GPU")
	assert.Equal(t, "检查实例状态与 GPU 配置", result.StoppedAt)
	assert.Len(t, executor.calls, 1)
}

func TestGPUChain_Running_GPUWorking(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId": "uhost-abc",
					"State":   "Running",
					"GPU":     float64(1),
					"GpuType": "4090",
				},
			},
		},
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{
				"List": []any{
					map[string]any{
						"UHostId": "uhost-abc",
						"Metrics": []any{
							map[string]any{
								"MetricKey": "cloudwatch_gpu_util",
								"Results": []any{
									map[string]any{
										"Values": []any{
											map[string]any{"Timestamp": float64(1712563200), "Value": float64(45.0)},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := GPUNotDetectedChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "GPU 硬件工作正常")
	assert.Contains(t, result.Suggestion, "控制台")
	assertReadOnlyDiagnosisSuggestion(t, result.Suggestion)
	assert.Equal(t, "检查 GPU 监控数据", result.StoppedAt)
	assert.Len(t, executor.calls, 2)
}

func TestGPUChain_Running_GPUNoMetrics_Fallback(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId": "uhost-abc",
					"State":   "Running",
					"GPU":     float64(1),
					"GpuType": "4090",
				},
			},
		},
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{
				"List": []any{
					map[string]any{
						"UHostId": "uhost-abc",
						"Metrics": []any{
							map[string]any{
								"MetricKey": "uhost_cpu_used",
								"Results": []any{
									map[string]any{
										"Values": []any{
											map[string]any{"Timestamp": float64(1712563200), "Value": float64(30.0)},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := GPUNotDetectedChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "监控未返回 GPU 数据")
	assert.Contains(t, result.Suggestion, "nvidia-smi")
	assert.Equal(t, "检查 GPU 监控数据", result.StoppedAt)
	assert.Len(t, executor.calls, 2)
	assert.Len(t, result.Steps, 2)
	assert.Equal(t, "concluded", result.Steps[1].Status)
}

func TestGPUChain_Running_GPUIdleDoesNotClaimDriverFailure(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId": "uhost-abc",
					"State":   "Running",
					"GPU":     float64(1),
					"GpuType": "4090",
				},
			},
		},
		"GetCompShareInstanceMonitor": {
			"Data": map[string]any{
				"List": []any{
					map[string]any{
						"UHostId": "uhost-abc",
						"Metrics": []any{
							map[string]any{
								"MetricKey": "cloudwatch_gpu_util",
								"Results": []any{
									map[string]any{"Values": []any{map[string]any{"Value": float64(0)}}},
								},
							},
							map[string]any{
								"MetricKey": "cloudwatch_gpu_memory_usage",
								"Results": []any{
									map[string]any{"Values": []any{map[string]any{"Value": float64(0)}}},
								},
							},
						},
					},
				},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := GPUNotDetectedChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "未看到 GPU 活动")
	assert.NotContains(t, result.Conclusion, "驱动未正确加载")
	assert.Contains(t, result.Suggestion, "nvidia-smi")
	assert.Empty(t, result.StoppedAt)
}

func TestGPUChain_InstallFail(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{"UHostId": "uhost-abc", "State": "Install Fail"},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := GPUNotDetectedChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-abc"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "初始化失败")
	assert.Contains(t, result.Suggestion, "删除重建")
	assert.Equal(t, "检查实例状态与 GPU 配置", result.StoppedAt)
	assert.Len(t, executor.calls, 1)
}

func TestGPUChain_NotFound(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{},
		},
	}}
	onStep, _ := collectEvents()

	chain := GPUNotDetectedChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{"UHostId": "uhost-xxx"})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "未找到")
	assert.Equal(t, "检查实例状态与 GPU 配置", result.StoppedAt)
	assert.Len(t, executor.calls, 1)
}
