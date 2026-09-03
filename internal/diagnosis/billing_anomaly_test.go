package diagnosis

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestBillingChain_MissingSpecifiedInstanceDoesNotAssertAccountAbsence(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}},
	}}
	onStep, _ := collectEvents()
	result, err := NewEngine(executor, onStep).Run(context.Background(), BillingAnomalyChain(), map[string]any{
		"UHostId": "uhost-not-found",
	})
	require.NoError(t, err)
	require.True(t, result.Success, "the point query completed without finding the requested instance")
	require.Contains(t, result.Conclusion, "未找到指定实例")
	require.Contains(t, result.Conclusion, "未取得其当前报价")
	require.Contains(t, result.Conclusion, "不能判断历史实际扣款")
	require.NotContains(t, result.Conclusion, "未找到任何实例")
	require.NotContains(t, result.Conclusion, "可能存在未释放的资源")
	require.Len(t, executor.calls, 1)
	require.Equal(t, []any{"uhost-not-found"}, executor.calls[0].args["UHostIds"])
}

func TestBillingChain_SingleRunning(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId":             "uhost-abc",
					"Name":                "my-gpu",
					"State":               "Running",
					"GpuType":             "4090",
					"GPU":                 float64(1),
					"ChargeType":          "Postpay",
					"InstancePrice":       float64(1.58),
					"DiskPrice":           float64(0.05),
					"DiskPriceInfo":       []any{diskPriceInfo("Postpay", 0.05, true)},
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

func TestBillingChain_MissingPriceFieldsAreUnknownNotZero(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId":     "uhost-missing-price",
					"Name":        "no-price",
					"State":       "Running",
					"GpuType":     "4090",
					"GPU":         float64(1),
					"ChargeType":  "Postpay",
					"DiskPrice":   float64(0),
					"Memory":      float64(65536),
					"CPU":         float64(16),
					"CompShareId": "cs-test",
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
	assert.Contains(t, result.Conclusion, "算力：未返回")
	assert.Contains(t, result.Conclusion, "镜像：未返回")
	assert.Contains(t, result.Conclusion, "部分当前费用缺少")
	assert.NotContains(t, result.Conclusion, "实例费 ¥0.00")
	assert.NotContains(t, result.Conclusion, "合计: ¥0.00/时")
}

func TestBillingChain_StoppedWithDiskCost(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId":                        "uhost-def",
					"Name":                           "idle-gpu",
					"State":                          "Stopped",
					"GpuType":                        "4090",
					"GPU":                            float64(1),
					"ChargeType":                     "Postpay",
					"InstancePrice":                  float64(0),
					"DiskPrice":                      float64(0.05),
					"DiskPriceInfo":                  []any{diskPriceInfo("Postpay", 0.05, true)},
					"PostPayPowerOffBillingResource": []any{diskPriceInfo("Postpay", 0.05, true)},
					"CompShareImagePrice":            float64(0),
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
	assert.Contains(t, result.Conclusion, "系统盘")
	assert.Contains(t, result.Conclusion, "关机后仍计费")
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
					"UHostId":             "uhost-img",
					"Name":                "sd-webui",
					"State":               "Running",
					"GpuType":             "4090",
					"GPU":                 float64(1),
					"ChargeType":          "Postpay",
					"InstancePrice":       float64(1.58),
					"DiskPrice":           float64(0.05),
					"DiskPriceInfo":       []any{diskPriceInfo("Postpay", 0.05, true)},
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
	assert.Contains(t, result.Conclusion, "镜像")
	assert.Contains(t, result.Conclusion, "0.30")
	// Total hourly should include instance + disk + image: 1.58 + 0.05 + 0.30 = 1.93
	assert.Contains(t, result.Conclusion, "1.93")
}

func TestBillingChain_StoppedPaidImage(t *testing.T) {
	// A paid image quote is present, but only the upstream power-off resource
	// breakdown is allowed to declare what keeps billing after shutdown.
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId":                        "uhost-si",
					"Name":                           "stopped-sd",
					"State":                          "Stopped",
					"GpuType":                        "4090",
					"GPU":                            float64(1),
					"ChargeType":                     "Postpay",
					"InstancePrice":                  float64(1.58),
					"DiskPrice":                      float64(0.05),
					"DiskPriceInfo":                  []any{diskPriceInfo("Postpay", 0.05, true)},
					"PostPayPowerOffBillingResource": []any{diskPriceInfo("Postpay", 0.05, true)},
					"CompShareImagePrice":            float64(0.30),
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
	// The API's power-off breakdown is authoritative: it lists the disk only.
	assert.Contains(t, result.Conclusion, "镜像")
	assert.Contains(t, result.Conclusion, "关机后仍计费")
	assert.Contains(t, result.Suggestion, "磁盘")
	assert.Contains(t, result.Conclusion, "¥0.05/时")
	assert.NotContains(t, result.Conclusion, "¥0.35", "must not invent stopped image retention outside the upstream breakdown")
}

func TestBillingChain_MixedInstances(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId":             "uhost-abc",
					"Name":                "running-gpu",
					"State":               "Running",
					"GpuType":             "4090",
					"GPU":                 float64(1),
					"ChargeType":          "Postpay",
					"InstancePrice":       float64(1.58),
					"DiskPrice":           float64(0.05),
					"DiskPriceInfo":       []any{diskPriceInfo("Postpay", 0.05, true)},
					"CompShareImagePrice": float64(0),
				},
				map[string]any{
					"UHostId":                        "uhost-def",
					"Name":                           "stopped-gpu",
					"State":                          "Stopped",
					"GpuType":                        "4090",
					"GPU":                            float64(1),
					"ChargeType":                     "Postpay",
					"InstancePrice":                  float64(0),
					"DiskPrice":                      float64(0.05),
					"DiskPriceInfo":                  []any{diskPriceInfo("Postpay", 0.05, true)},
					"PostPayPowerOffBillingResource": []any{diskPriceInfo("Postpay", 0.05, true)},
					"CompShareImagePrice":            float64(0),
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
	assert.Contains(t, result.Conclusion, "关机后仍计费")
	assert.Contains(t, result.Suggestion, "释放")
	assert.Equal(t, "查询价格详情", result.StoppedAt)
	assert.Len(t, executor.calls, 2)
	// step2 should pass both IDs
	ids, ok := executor.calls[1].args["UHostIds"].([]any)
	assert.True(t, ok)
	assert.Contains(t, ids, "uhost-abc")
	assert.Contains(t, ids, "uhost-def")
}

func diskPriceInfo(chargeType string, price float64, isBoot bool) map[string]any {
	return map[string]any{"ChargeType": chargeType, "Price": price, "IsBoot": isBoot}
}

func TestBillingChain_SpecificInstance(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"UHostSet": []any{
				map[string]any{
					"UHostId":             "uhost-xyz",
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

func TestBillingChainPaginatesListAndPriceBatches(t *testing.T) {
	executor := &billingPagingExecutor{}
	onStep, _ := collectEvents()

	result, err := NewEngine(executor, onStep).Run(context.Background(), BillingAnomalyChain(), nil)
	require.NoError(t, err)
	assert.Contains(t, result.Conclusion, "查到 101 个实例")
	assert.NotContains(t, result.Conclusion, "不能据此计算全账号合计")
	assert.Equal(t, 4, executor.calls, "101 instances require two list pages and two price batches")
}

type billingPagingExecutor struct{ calls int }

func (e *billingPagingExecutor) Execute(_ context.Context, _ string, args map[string]any) (map[string]any, error) {
	e.calls++
	if ids, ok := args["UHostIds"].([]any); ok {
		rows := make([]any, 0, len(ids))
		for _, rawID := range ids {
			rows = append(rows, pricedBillingHost(fmt.Sprint(rawID)))
		}
		return map[string]any{"TotalCount": float64(len(rows)), "UHostSet": rows}, nil
	}
	offset, _ := args["Offset"].(int)
	start, end := offset, offset+100
	if end > 101 {
		end = 101
	}
	rows := make([]any, 0, end-start)
	for i := start; i < end; i++ {
		rows = append(rows, map[string]any{"UHostId": fmt.Sprintf("uhost-%03d", i)})
	}
	return map[string]any{"TotalCount": float64(101), "UHostSet": rows}, nil
}

func pricedBillingHost(id string) map[string]any {
	return map[string]any{
		"UHostId": id, "Name": id, "State": "Running", "ChargeType": "Postpay",
		"InstancePrice": float64(1), "CompShareImagePrice": float64(0),
		"DiskPriceInfo": []any{diskPriceInfo("Postpay", 0, true)},
	}
}
