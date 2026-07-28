package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/deployment"
)

// withCFSZone attaches a one-zone catalog snapshot as the turn's reference data, so
// CreateCFSWorkflow resolves the Pod-zone placement from it (the second support-zone
// query was removed in the zone convergence). Zone params are canonical ids;
// display-name→id resolution now lives in the action resolver, upstream of the workflow.
func withCFSZone(zone, region, describe string, zoneID, azGroup uint32, isPod bool) RunOption {
	return WithReferenceData(ReferenceData{ZoneCatalog: deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: zone, Region: region, ZoneID: zoneID, AzGroup: azGroup, IsPod: isPod}, DisplayName: describe},
	})})
}

func cfsDescribeResult() map[string]any {
	return map[string]any{
		"CFSSet": []any{
			map[string]any{
				"CfsId":       "cfs-test",
				"Found":       true,
				"Name":        "shared-train",
				"ZoneId":      float64(9001),
				"Size":        float64(100),
				"ChargeType":  "Month",
				"MountStatus": "Mounted",
			},
		},
	}
}

func cfsSupportZone(zone, region, describe string, zoneID, regionID float64, isPod bool) map[string]any {
	return map[string]any{"ZoneInfo": []any{
		map[string]any{
			"Zone":     zone,
			"Region":   region,
			"ZoneId":   zoneID,
			"RegionId": regionID,
			"Describe": describe,
			"IsPod":    isPod,
		},
	}}
}

func TestCreateCFSWorkflowRequiresZoneBeforeConfirm(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareSupportZone": cfsSupportZone("cn-bj2-03", "cn-bj2", "华北一C", 9103, 3103, true),
		"GetCompShareCFSPrice":         {"Price": float64(99)},
		"CreateCFS":                    {"CfsId": "cfs-new"},
	}}
	onStep, events := collectEvents()
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("missing zone must be blocked before confirmation")
		return true
	}, onStep)

	result, err := eng.Run(context.Background(), CreateCFSDef(), map[string]any{
		"Name": "shared-train",
		"Size": float64(100),
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "可用区")
	for _, ev := range *events {
		assert.NotEqual(t, StepConfirm, ev.Type)
	}
	_, created := findExecutorCall(executor.calls, "CreateCFS")
	assert.False(t, created)
}

func TestCreateCFSWorkflowConfirmsBeforeCreate(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareSupportZone": cfsSupportZone("cn-bj2-03", "cn-bj2", "华北一C", 9103, 3103, true),
		// Real GetCompShareCFSPrice shape: no flat Price; payable value is in
		// PriceDetails[0].Disks (upstream pod/get_compshare_cfs_price.go).
		"GetCompShareCFSPrice": {
			"PriceDetails": []any{map[string]any{"ChargeType": "Month", "Disks": float64(99)}},
		},
		"CreateCFS": {"CfsId": "cfs-new", "Name": "shared-train", "Size": float64(100)},
	}}
	var confirmed bool
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		confirmed = true
		assert.Equal(t, "CreateCFSWorkflow", action)
		assert.Equal(t, "shared-train", args["Name"])
		assert.Equal(t, float64(100), args["Size"])
		assert.Equal(t, "cn-bj2-03", args["Zone"])
		// Confirm card must carry a formatted price STRING, not the raw
		// PriceDetails array (which would render as "[object Object]").
		assert.Equal(t, "¥99.00", args["price"])
		return true
	}, nil)

	result, err := eng.Run(context.Background(), CreateCFSDef(), map[string]any{
		"Name":                "shared-train",
		"Size":                float64(100),
		"Zone":                "cn-bj2-03",
		"ChargeType":          "Month",
		"top_organization_id": uint32(101),
		"organization_id":     uint32(202),
	}, withCFSZone("cn-bj2-03", "cn-bj2", "华北一C", 9103, 3103, true))

	require.NoError(t, err)
	assert.True(t, result.Success)
	assert.True(t, confirmed)
	priceCall, ok := findExecutorCall(executor.calls, "GetCompShareCFSPrice")
	require.True(t, ok)
	assert.NotContains(t, priceCall.args, "Zone", "Pod CFS price uses the backend placement id only")
	assert.NotContains(t, priceCall.args, "Region")
	assert.Equal(t, uint32(9103), priceCall.args["zone_id"])
	assert.Equal(t, uint32(3103), priceCall.args["az_group"])
	createCall, ok := findExecutorCall(executor.calls, "CreateCFS")
	require.True(t, ok)
	assert.Equal(t, "cn-bj2-03", createCall.args["Zone"], "deployed gateway still routes CreateCFS by public zone")
	assert.Equal(t, "cn-bj2", createCall.args["Region"])
	assert.Equal(t, uint32(9103), createCall.args["zone_id"])
	assert.Equal(t, uint32(3103), createCall.args["az_group"])
	assert.Equal(t, uint32(101), createCall.args["top_organization_id"])
	assert.Equal(t, uint32(202), createCall.args["organization_id"])
	assert.Equal(t, "Month", createCall.args["ChargeType"])
	assert.Equal(t, float64(100), createCall.args["Size"])
}

func TestCreateCFSWorkflowRejectsExistingSameZoneBeforePriceAndConfirmation(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCFS": cfsDescribeResult(),
		"GetCompShareCFSPrice": {
			"PriceDetails": []any{map[string]any{"ChargeType": "Month", "Disks": float64(99)}},
		},
		"CreateCFS": {"CfsId": "must-not-be-created"},
	}}
	eng := NewEngine(executor, func(string, map[string]any) bool {
		t.Fatal("同区已有 CFS 时不能进入确认")
		return true
	}, nil)

	result, err := eng.Run(context.Background(), CreateCFSDef(), map[string]any{
		"Name":       "duplicate-cfs",
		"Size":       float64(100),
		"Zone":       "cn-bj2-03",
		"ChargeType": "Month",
	}, withCFSZone("cn-bj2-03", "cn-bj2", "华北一C", 9001, 3103, true))

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "已经有 CFS")
	assert.Contains(t, result.Message, "cfs-test")
	_, priced := findExecutorCall(executor.calls, "GetCompShareCFSPrice")
	assert.False(t, priced)
	_, created := findExecutorCall(executor.calls, "CreateCFS")
	assert.False(t, created)

	preflight, ok := findExecutorCall(executor.calls, "DescribeCFS")
	require.True(t, ok)
	assert.Equal(t, uint32(9001), preflight.args["zone_id"])
	assert.Equal(t, "cn-bj2-03", preflight.args["Zone"])
	assert.Equal(t, "cn-bj2", preflight.args["Region"])
}

func TestCreateCFSWorkflowRejectsUnknownZoneBeforePrice(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareSupportZone": cfsSupportZone("cn-bj2-03", "cn-bj2", "华北一C", 9103, 3103, true),
		"GetCompShareCFSPrice":         {"PriceDetails": []any{map[string]any{"ChargeType": "Month", "Disks": float64(99)}}},
		"CreateCFS":                    {"CfsId": "cfs-new"},
	}}
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("unknown CFS zone must be blocked before confirmation")
		return true
	}, nil)

	result, err := eng.Run(context.Background(), CreateCFSDef(), map[string]any{
		"Name":       "shared-train",
		"Size":       float64(100),
		"Zone":       "cn-pod-01",
		"ChargeType": "Month",
	}, withCFSZone("cn-bj2-03", "cn-bj2", "华北一C", 9103, 3103, true))

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "不在当前可用区目录中")
	assert.NotContains(t, result.Message, "zone_id")
	_, priced := findExecutorCall(executor.calls, "GetCompShareCFSPrice")
	assert.False(t, priced)
	_, created := findExecutorCall(executor.calls, "CreateCFS")
	assert.False(t, created)
}

func TestCreateCFSWorkflowBlocksWhenPriceMissingBeforeConfirm(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareSupportZone": cfsSupportZone("cn-bj2-03", "cn-bj2", "华北一C", 9103, 3103, true),
		"GetCompShareCFSPrice":         {"RetCode": 0},
		"CreateCFS":                    {"CfsId": "cfs-new"},
	}}
	onStep, events := collectEvents()
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("missing CFS create price must be blocked before confirmation")
		return true
	}, onStep)

	result, err := eng.Run(context.Background(), CreateCFSDef(), map[string]any{
		"Name":       "shared-train",
		"Size":       float64(100),
		"Zone":       "cn-bj2-03",
		"ChargeType": "Month",
	}, withCFSZone("cn-bj2-03", "cn-bj2", "华北一C", 9103, 3103, true))

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "未获取到价格")
	for _, ev := range *events {
		assert.False(t, ev.Type == StepConfirm && ev.Status == "waiting")
	}
	_, created := findExecutorCall(executor.calls, "CreateCFS")
	assert.False(t, created)
}

func TestCreateCFSWorkflowRejectsNonPodZoneBeforePrice(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareSupportZone": cfsSupportZone("cn-wlcb-01", "cn-wlcb", "华北二A", 10027, 2001, false),
		"GetCompShareCFSPrice":         {"Price": float64(99)},
		"CreateCFS":                    {"CfsId": "cfs-new"},
	}}
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("non-pod CFS create must be blocked before confirmation")
		return true
	}, nil)

	result, err := eng.Run(context.Background(), CreateCFSDef(), map[string]any{
		"Name": "shared-train",
		"Size": float64(100),
		"Zone": "cn-wlcb-01",
	}, withCFSZone("cn-wlcb-01", "cn-wlcb", "华北二A", 10027, 2001, false))

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "只支持 Pod")
	_, priced := findExecutorCall(executor.calls, "GetCompShareCFSPrice")
	assert.False(t, priced)
	_, created := findExecutorCall(executor.calls, "CreateCFS")
	assert.False(t, created)
}

func TestCreateCFSWorkflowRequiresZoneIDBeforePrice(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareSupportZone": cfsSupportZone("cn-bj2-03", "cn-bj2", "华北一C", 0, 3103, true),
		"GetCompShareCFSPrice":         {"Price": float64(99)},
		"CreateCFS":                    {"CfsId": "cfs-new"},
	}}
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("missing zone_id must be blocked before confirmation")
		return true
	}, nil)

	result, err := eng.Run(context.Background(), CreateCFSDef(), map[string]any{
		"Name": "shared-train",
		"Size": float64(100),
		"Zone": "cn-bj2-03",
	}, withCFSZone("cn-bj2-03", "cn-bj2", "华北一C", 0, 3103, true))

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "内部编号")
	_, priced := findExecutorCall(executor.calls, "GetCompShareCFSPrice")
	assert.False(t, priced)
}

func TestCreateCFSWorkflowRequiresAzGroupBeforePrice(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareSupportZone": cfsSupportZone("cn-bj2-03", "cn-bj2", "华北一C", 9103, 0, true),
		"GetCompShareCFSPrice":         {"Price": float64(99)},
		"CreateCFS":                    {"CfsId": "cfs-new"},
	}}
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("missing az_group must be blocked before confirmation")
		return true
	}, nil)

	result, err := eng.Run(context.Background(), CreateCFSDef(), map[string]any{
		"Name": "shared-train",
		"Size": float64(100),
		"Zone": "cn-bj2-03",
	}, withCFSZone("cn-bj2-03", "cn-bj2", "华北一C", 9103, 0, true))

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "内部区域编号")
	_, priced := findExecutorCall(executor.calls, "GetCompShareCFSPrice")
	assert.False(t, priced)
}

func TestResizeCFSWorkflowUsesDescribeZoneIDInternally(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCFS":                 cfsDescribeResult(),
		"GetCompShareCFSUpgradePrice": {"Price": float64(49), "OriginalPrice": float64(60)},
		"ResizeCFS":                   {"CfsId": "cfs-test", "OldSize": float64(100), "NewSize": float64(200)},
	}}
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		assert.Equal(t, "ResizeCFSWorkflow", action)
		assert.Equal(t, "cfs-test", args["CfsId"])
		assert.Equal(t, float64(100), args["current_size_gb"])
		assert.Equal(t, float64(200), args["target_size_gb"])
		return true
	}, nil)

	result, err := eng.Run(context.Background(), ResizeCFSDef(), map[string]any{
		"CfsId":               "cfs-test",
		"Size":                float64(200),
		"top_organization_id": uint32(101),
		"organization_id":     uint32(202),
	})

	require.NoError(t, err)
	assert.True(t, result.Success)
	priceCall, ok := findExecutorCall(executor.calls, "GetCompShareCFSUpgradePrice")
	require.True(t, ok)
	assert.Equal(t, float64(9001), priceCall.args["zone_id"])
	assert.Equal(t, uint32(101), priceCall.args["top_organization_id"])
	assert.Equal(t, uint32(202), priceCall.args["organization_id"])
	resizeCall, ok := findExecutorCall(executor.calls, "ResizeCFS")
	require.True(t, ok)
	assert.Equal(t, float64(9001), resizeCall.args["zone_id"])
	assert.Equal(t, uint32(101), resizeCall.args["top_organization_id"])
	assert.Equal(t, uint32(202), resizeCall.args["organization_id"])
	assert.Equal(t, float64(200), resizeCall.args["Size"])
}

func TestResizeCFSWorkflowBlocksWhenPriceMissingBeforeConfirm(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCFS":                 cfsDescribeResult(),
		"GetCompShareCFSUpgradePrice": {"RetCode": 0},
		"ResizeCFS":                   {"CfsId": "cfs-test", "OldSize": float64(100), "NewSize": float64(200)},
	}}
	onStep, events := collectEvents()
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("missing CFS resize price must be blocked before confirmation")
		return true
	}, onStep)

	result, err := eng.Run(context.Background(), ResizeCFSDef(), map[string]any{
		"CfsId": "cfs-test",
		"Size":  float64(200),
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "未获取到价格")
	for _, ev := range *events {
		assert.False(t, ev.Type == StepConfirm && ev.Status == "waiting")
	}
	_, resized := findExecutorCall(executor.calls, "ResizeCFS")
	assert.False(t, resized)
}

func TestResizeCFSWorkflowBlocksShrinkBeforeConfirm(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCFS": cfsDescribeResult(),
	}}
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("shrink must be blocked before confirmation")
		return true
	}, nil)

	result, err := eng.Run(context.Background(), ResizeCFSDef(), map[string]any{
		"CfsId": "cfs-test",
		"Size":  float64(80),
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "大于当前容量")
	_, resized := findExecutorCall(executor.calls, "ResizeCFS")
	assert.False(t, resized)
}

func TestEnableNetOptimizerWorkflowConfirmsBeforeSync(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"CheckCompShareNetOptimizer": {"Optimized": false},
		"SyncCompShareNetOptimizer":  {"RetCode": float64(0)},
	}}
	var confirmed bool
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		confirmed = true
		assert.Equal(t, "EnableNetOptimizerWorkflow", action)
		assert.Equal(t, false, args["optimized"])
		return true
	}, nil)

	result, err := eng.Run(context.Background(), EnableNetOptimizerDef(), map[string]any{
		"Zone":                "cn-bj2-03",
		"top_organization_id": uint32(101),
		"organization_id":     uint32(202),
	}, withPodZone("cn-bj2-03", "cn-bj2", 5001, 3001))

	require.NoError(t, err)
	assert.True(t, result.Success)
	assert.True(t, confirmed)
	checkCall, ok := findExecutorCall(executor.calls, "CheckCompShareNetOptimizer")
	require.True(t, ok)
	assert.Equal(t, uint32(3001), checkCall.args["az_group"])
	syncCall, synced := findExecutorCall(executor.calls, "SyncCompShareNetOptimizer")
	require.True(t, synced)
	assert.Equal(t, uint32(3001), syncCall.args["az_group"])
	assert.Equal(t, uint32(101), syncCall.args["top_organization_id"])
	assert.Equal(t, uint32(202), syncCall.args["organization_id"])
}

func TestEnableNetOptimizerWorkflowSkipsSyncWhenAlreadyEnabled(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"CheckCompShareNetOptimizer": {"Optimized": true},
	}}
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("already-enabled status should not ask for confirmation")
		return true
	}, nil)

	result, err := eng.Run(context.Background(), EnableNetOptimizerDef(), map[string]any{
		"Zone": "cn-bj2-03",
	}, withPodZone("cn-bj2-03", "cn-bj2", 5001, 3001))

	require.NoError(t, err)
	assert.True(t, result.Success)
	assert.Equal(t, true, result.Data["already_optimized"])
	_, synced := findExecutorCall(executor.calls, "SyncCompShareNetOptimizer")
	assert.False(t, synced)
}

func TestEnableNetOptimizerWorkflowRequiresZoneBeforeConfirm(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"CheckCompShareNetOptimizer": {"Optimized": false},
	}}
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("missing zone must be blocked before confirmation")
		return true
	}, nil)

	result, err := eng.Run(context.Background(), EnableNetOptimizerDef(), nil)

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "可用区")
	_, checked := findExecutorCall(executor.calls, "CheckCompShareNetOptimizer")
	assert.False(t, checked)
}
