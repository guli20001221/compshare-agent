package diagnosis

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBillingChain_NoInstances(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{},
		},
	}}
	onStep, _ := collectEvents()

	chain := BillingAnomalyChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, nil)

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "未找到任何实例")
	assert.Contains(t, result.Suggestion, "控制台")
	assert.Equal(t, "查询实例计费信息", result.StoppedAt)
	assert.Len(t, result.Steps, 1)
	assert.Equal(t, "concluded", result.Steps[0].Status)
	assert.Len(t, executor.calls, 1)
}

func TestBillingChain_SingleRunning(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId":       "uhost-abc",
					"Name":          "my-gpu",
					"State":         "Running",
					"GpuType":       "4090",
					"GPU":           float64(1),
					"ChargeType":    "Dynamic",
					"InstancePrice": float64(1.58),
					"DiskPrice":     float64(0.05),
					"IsExpire":      "No",
				},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := BillingAnomalyChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, nil)

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "uhost-abc")
	assert.Contains(t, result.Conclusion, "1.58")
	assert.Contains(t, result.Conclusion, "0.05")
	assert.Contains(t, result.Conclusion, "1 个实例")
	assert.Equal(t, "查询实例计费信息", result.StoppedAt)
	assert.Len(t, executor.calls, 1)
}

func TestBillingChain_StoppedWithDiskCost(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId":       "uhost-def",
					"Name":          "idle-gpu",
					"State":         "Stopped",
					"GpuType":       "4090",
					"GPU":           float64(1),
					"ChargeType":    "Dynamic",
					"InstancePrice": float64(0),
					"DiskPrice":     float64(0.05),
					"IsExpire":      "No",
				},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := BillingAnomalyChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, nil)

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "关机")
	assert.Contains(t, result.Conclusion, "磁盘费用")
	assert.Contains(t, result.Suggestion, "释放")
	assert.Equal(t, "查询实例计费信息", result.StoppedAt)
	assert.Len(t, executor.calls, 1)
}

func TestBillingChain_MixedInstances(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId":       "uhost-abc",
					"Name":          "running-gpu",
					"State":         "Running",
					"GpuType":       "4090",
					"GPU":           float64(1),
					"ChargeType":    "Dynamic",
					"InstancePrice": float64(1.58),
					"DiskPrice":     float64(0.05),
					"IsExpire":      "No",
				},
				map[string]any{
					"UHostId":       "uhost-def",
					"Name":          "stopped-gpu",
					"State":         "Stopped",
					"GpuType":       "4090",
					"GPU":           float64(1),
					"ChargeType":    "Dynamic",
					"InstancePrice": float64(0),
					"DiskPrice":     float64(0.05),
					"IsExpire":      "No",
				},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := BillingAnomalyChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, nil)

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "2 个实例")
	assert.Contains(t, result.Conclusion, "uhost-abc")
	assert.Contains(t, result.Conclusion, "uhost-def")
	assert.Contains(t, result.Conclusion, "总计")
	assert.Contains(t, result.Conclusion, "关机")
	assert.Contains(t, result.Conclusion, "磁盘费用")
	assert.Contains(t, result.Suggestion, "释放")
	assert.Equal(t, "查询实例计费信息", result.StoppedAt)
	assert.Len(t, executor.calls, 1)
}

func TestBillingChain_SpecificInstance(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId":       "uhost-xyz",
					"Name":          "target-gpu",
					"State":         "Running",
					"GpuType":       "A100",
					"GPU":           float64(2),
					"ChargeType":    "Month",
					"InstancePrice": float64(5.00),
					"DiskPrice":     float64(0.10),
					"IsExpire":      "No",
				},
			},
		},
	}}
	onStep, _ := collectEvents()

	chain := BillingAnomalyChain()
	eng := NewEngine(executor, onStep)
	result, err := eng.Run(context.Background(), chain, map[string]any{
		"UHostId": "uhost-xyz",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Contains(t, result.Conclusion, "uhost-xyz")
	assert.Equal(t, "查询实例计费信息", result.StoppedAt)
	assert.Len(t, executor.calls, 1)

	// Verify BuildArgs passed the UHostIds filter
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
	args := executor.calls[0].args
	uhostIDs, ok := args["UHostIds"].([]any)
	assert.True(t, ok)
	assert.Equal(t, []any{"uhost-xyz"}, uhostIDs)
}
