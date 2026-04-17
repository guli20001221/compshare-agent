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
	assert.Equal(t, "查询实例列表", result.StoppedAt)
	assert.Len(t, executor.calls, 1)
}

func TestBillingChain_SingleRunning(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId":              "uhost-abc",
					"Name":                "my-gpu",
					"State":               "Running",
					"GpuType":             "4090",
					"GPU":                 float64(1),
					"ChargeType":          "Dynamic",
					"InstancePrice":       float64(1.58),
					"DiskPrice":           float64(0.05),
					"CompShareImagePrice": float64(0), // free platform image
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
	assert.NotContains(t, result.Conclusion, "镜像费", "free image should not show image cost line")
	// 2 API calls: step1 list (no IDs) + step2 with IDs for pricing
	assert.Equal(t, "查询价格详情", result.StoppedAt)
	assert.Len(t, executor.calls, 2)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[1].action)
	// step2 should pass UHostIds
	ids, ok := executor.calls[1].args["UHostIds"].([]any)
	assert.True(t, ok)
	assert.Equal(t, []any{"uhost-abc"}, ids)
}

func TestBillingChain_StoppedWithDiskCost(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId":              "uhost-def",
					"Name":                "idle-gpu",
					"State":               "Stopped",
					"GpuType":             "4090",
					"GPU":                 float64(1),
					"ChargeType":          "Dynamic",
					"InstancePrice":       float64(0),
					"DiskPrice":           float64(0.05),
					"CompShareImagePrice": float64(0),
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
	assert.Contains(t, result.Conclusion, "磁盘保留费用")
	assert.NotContains(t, result.Conclusion, "镜像保留费用", "free image should not mention image cost in warning")
	assert.Contains(t, result.Suggestion, "释放")
	assert.NotContains(t, result.Suggestion, "镜像", "free image should not mention image in suggestion")
	assert.Equal(t, "查询价格详情", result.StoppedAt)
	assert.Len(t, executor.calls, 2)
}

func TestBillingChain_PaidCommunityImage(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId":              "uhost-img",
					"Name":                "sd-webui",
					"State":               "Running",
					"GpuType":             "4090",
					"GPU":                 float64(1),
					"ChargeType":          "Dynamic",
					"InstancePrice":       float64(1.58),
					"DiskPrice":           float64(0.05),
					"CompShareImagePrice": float64(0.30),
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
	assert.Contains(t, result.Conclusion, "镜像费")
	assert.Contains(t, result.Conclusion, "0.30")
	// Total hourly should include instance + disk + image: 1.58 + 0.05 + 0.30 = 1.93
	assert.Contains(t, result.Conclusion, "1.93")
}

func TestBillingChain_StoppedPaidImage(t *testing.T) {
	// Stopped instance with paid community image — image keeps billing
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId":              "uhost-si",
					"Name":                "stopped-sd",
					"State":               "Stopped",
					"GpuType":             "4090",
					"GPU":                 float64(1),
					"ChargeType":          "Dynamic",
					"InstancePrice":       float64(1.58),
					"DiskPrice":           float64(0.05),
					"CompShareImagePrice": float64(0.30),
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
	// Stopped + paid image: keep cost = disk + image
	assert.Contains(t, result.Conclusion, "镜像")
	assert.Contains(t, result.Conclusion, "保留费用")
	assert.Contains(t, result.Suggestion, "镜像")
	// Hourly cost for stopped Dynamic: instance=0 + disk=0.05 + image=0.30 = 0.35
	assert.Contains(t, result.Conclusion, "0.35")
}

func TestBillingChain_MixedInstances(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId":              "uhost-abc",
					"Name":                "running-gpu",
					"State":               "Running",
					"GpuType":             "4090",
					"GPU":                 float64(1),
					"ChargeType":          "Dynamic",
					"InstancePrice":       float64(1.58),
					"DiskPrice":           float64(0.05),
					"CompShareImagePrice": float64(0),
				},
				map[string]any{
					"UHostId":              "uhost-def",
					"Name":                "stopped-gpu",
					"State":               "Stopped",
					"GpuType":             "4090",
					"GPU":                 float64(1),
					"ChargeType":          "Dynamic",
					"InstancePrice":       float64(0),
					"DiskPrice":           float64(0.05),
					"CompShareImagePrice": float64(0),
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
	assert.Contains(t, result.Conclusion, "合计")
	assert.Contains(t, result.Conclusion, "关机")
	assert.Contains(t, result.Conclusion, "保留费用")
	assert.Contains(t, result.Suggestion, "释放")
	assert.Equal(t, "查询价格详情", result.StoppedAt)
	assert.Len(t, executor.calls, 2)
	// step2 should pass both IDs
	ids, ok := executor.calls[1].args["UHostIds"].([]any)
	assert.True(t, ok)
	assert.Contains(t, ids, "uhost-abc")
	assert.Contains(t, ids, "uhost-def")
}

func TestBillingChain_SpecificInstance(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId":              "uhost-xyz",
					"Name":                "target-gpu",
					"State":               "Running",
					"GpuType":             "A100",
					"GPU":                 float64(2),
					"ChargeType":          "Month",
					"InstancePrice":       float64(5.00),
					"DiskPrice":           float64(0.10),
					"CompShareImagePrice": float64(0),
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
	// When UHostId is specified, step1 queries with IDs directly and concludes
	assert.Equal(t, "查询实例列表", result.StoppedAt)
	assert.Len(t, executor.calls, 1) // only 1 call (step1 concludes early)

	// Verify BuildArgs passed the UHostIds filter
	args := executor.calls[0].args
	uhostIDs, ok := args["UHostIds"].([]any)
	assert.True(t, ok)
	assert.Equal(t, []any{"uhost-xyz"}, uhostIDs)
}
