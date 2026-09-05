package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResizeCapacity_VMUsesObservedInstanceAndZoneFacts(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":    stoppedInstanceResult(),
		"DescribeCompShareSupportZone": resizeSupportZonesResult("cn-sh2-02", "cn-sh2", 2002, 1002, false),
		"DescribeAvailableCompShareInstanceTypes": resizeInstanceTypesResult("4090", "cn-sh2-02", 2,
			specCandidate{CPU: 16, MemoryMB: 65536}),
		"CheckCompShareResourceCapacity":   resizeCapacityResult(2, 16, 64, true),
		"GetCompShareInstanceUpgradePrice": {"Price": float64(1.5)},
		"ResizeCompShareInstance":          {"RetCode": 0},
	}}
	result, err := NewEngine(executor, func(string, map[string]any) bool { return true }, nil).Run(
		context.Background(), ResizeInstanceDef(), map[string]any{"UHostId": "uhost-test", "Gpu": float64(2)})

	require.NoError(t, err)
	require.True(t, result.Success)
	call, ok := findExecutorCall(executor.calls, "CheckCompShareResourceCapacity")
	require.True(t, ok)
	assert.Equal(t, "uhost-test", call.args["UHostId"])
	assert.Equal(t, "4090", call.args["GpuType"])
	assert.Equal(t, "G", call.args["MachineType"])
	assert.Equal(t, "Amd/Epyc2", call.args["MinimalCpuPlatform"])
	assert.Equal(t, "Dynamic", call.args["ChargeType"])
	assert.Equal(t, "img-001", call.args["CompShareImageId"])
	assert.Equal(t, "cn-sh2-02", call.args["Zone"])
	assert.Equal(t, "cn-sh2", call.args["Region"])
	assert.Equal(t, uint32(2002), call.args["zone_id"])
	assert.NotContains(t, call.args, "IsPod")
	require.Equal(t, []any{map[string]any{"IsBoot": true, "Type": "CLOUD_SSD", "Size": uint32(100)}}, call.args["Disks"])
}

func TestResizeCapacity_VMDoesNotDependOnSupportZoneCatalog(t *testing.T) {
	executor := &mockExecutor{
		failOn: "DescribeCompShareSupportZone",
		results: map[string]map[string]any{
			"DescribeCompShareInstance": stoppedInstanceResult(),
			"DescribeAvailableCompShareInstanceTypes": resizeInstanceTypesResult("4090", "cn-sh2-02", 2,
				specCandidate{CPU: 16, MemoryMB: 65536}),
			"CheckCompShareResourceCapacity":   resizeCapacityResult(2, 16, 64, true),
			"GetCompShareInstanceUpgradePrice": {"Price": float64(1.5)},
			"ResizeCompShareInstance":          {"RetCode": 0},
		},
	}
	result, err := NewEngine(executor, func(string, map[string]any) bool { return true }, nil).Run(
		context.Background(), ResizeInstanceDef(), map[string]any{"UHostId": "uhost-test", "Gpu": float64(2)})

	require.NoError(t, err)
	require.True(t, result.Success)
	call, ok := findExecutorCall(executor.calls, "CheckCompShareResourceCapacity")
	require.True(t, ok)
	assert.Equal(t, "cn-sh2-02", call.args["Zone"])
	assert.Equal(t, "cn-sh2", call.args["Region"])
	assert.NotContains(t, call.args, "zone_id")
}

func TestResizeCapacity_CPodConfirmsTheCompleteOfferedTarget(t *testing.T) {
	instance := podStoppedInstanceResult()
	host := firstUHost(instance)
	host["CPU"], host["Memory"], host["ChargeType"] = float64(14), float64(32768), "Postpay"
	host["DiskSet"].([]any)[0].(map[string]any)["DiskType"] = "CVolume"
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":    instance,
		"DescribeCompShareSupportZone": resizeSupportZonesResult("cn-pod-01", "cn-pod", 9001, 3001, true),
		"DescribeAvailableCompShareInstanceTypes": resizeInstanceTypesResult("4090", "cn-pod-01", 1,
			specCandidate{CPU: 14, MemoryMB: 49152}),
		"CheckCompShareResourceCapacity":   resizeCapacityResult(1, 14, 48, true),
		"GetCompShareInstanceUpgradePrice": {"Price": float64(0)},
		"ResizeCompShareInstance":          {"RetCode": 0},
	}}
	confirmCalls := 0
	result, err := NewEngine(executor, func(_ string, summary map[string]any) bool {
		confirmCalls++
		assert.Equal(t, float64(14), summary["target_cpu"])
		assert.Equal(t, float64(1), summary["target_gpu"])
		assert.Equal(t, float64(49152), summary["target_memory"])
		return true
	}, nil).Run(context.Background(), ResizeInstanceDef(), map[string]any{"UHostId": "cpod-test", "Memory": float64(49152)})

	require.NoError(t, err)
	require.True(t, result.Success, result.Message)
	assert.Equal(t, 1, confirmCalls)
	for _, action := range []string{"CheckCompShareResourceCapacity", "GetCompShareInstanceUpgradePrice", "ResizeCompShareInstance"} {
		call, ok := findExecutorCall(executor.calls, action)
		require.True(t, ok, action)
		assert.Equal(t, uint32(9001), call.args["zone_id"], action)
	}
	price, _ := findExecutorCall(executor.calls, "GetCompShareInstanceUpgradePrice")
	write, _ := findExecutorCall(executor.calls, "ResizeCompShareInstance")
	assert.Equal(t, price.args["CPU"], write.args["Cpu"])
	assert.Equal(t, price.args["GPU"], write.args["Gpu"])
	assert.Equal(t, price.args["Memory"], write.args["Memory"])
}

func TestResizeCapacity_UHostContainerIsNotTreatedAsPod(t *testing.T) {
	wfCtx := NewContext(map[string]any{"UHostId": "uhost-container-test", "Gpu": float64(2)})
	outcome := stepQueryForResize().CheckResult(wfCtx, containerStoppedInstanceResult())

	assert.True(t, outcome.OK)
	assert.NotContains(t, outcome.Message, "cpod-")
}

func TestResizeCapacity_SoldOutStopsBeforePriceAndConfirmation(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":    stoppedInstanceResult(),
		"DescribeCompShareSupportZone": resizeSupportZonesResult("cn-sh2-02", "cn-sh2", 2002, 1002, false),
		"DescribeAvailableCompShareInstanceTypes": resizeInstanceTypesResult("4090", "cn-sh2-02", 2,
			specCandidate{CPU: 16, MemoryMB: 65536}),
		"CheckCompShareResourceCapacity": resizeCapacityResult(2, 16, 64, false),
	}}
	confirmed := false
	result, err := NewEngine(executor, func(string, map[string]any) bool {
		confirmed = true
		return true
	}, nil).Run(context.Background(), ResizeInstanceDef(), map[string]any{"UHostId": "uhost-test", "Gpu": float64(2)})

	require.NoError(t, err)
	require.False(t, result.Success)
	assert.Equal(t, "检查目标规格库存", result.StoppedAt)
	require.NotNil(t, result.Failure)
	assert.Equal(t, ReasonCapacitySoldOut, result.Failure.Reason)
	assert.False(t, confirmed)
	_, priced := findExecutorCall(executor.calls, "GetCompShareInstanceUpgradePrice")
	assert.False(t, priced)
	_, resized := findExecutorCall(executor.calls, "ResizeCompShareInstance")
	assert.False(t, resized)
}

func TestResizeCapacity_MissingOrForeignResponseDoesNotPretendTargetIsAvailable(t *testing.T) {
	for _, tt := range []struct {
		name     string
		capacity map[string]any
		message  string
	}{
		{name: "missing specs", capacity: map[string]any{"RetCode": 0}, message: "未返回可用规格"},
		{name: "another tuple", capacity: resizeCapacityResult(1, 16, 64, true), message: "未返回目标变配规格"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			executor := &mockExecutor{results: map[string]map[string]any{
				"DescribeCompShareInstance":    stoppedInstanceResult(),
				"DescribeCompShareSupportZone": resizeSupportZonesResult("cn-sh2-02", "cn-sh2", 2002, 1002, false),
				"DescribeAvailableCompShareInstanceTypes": resizeInstanceTypesResult("4090", "cn-sh2-02", 2,
					specCandidate{CPU: 16, MemoryMB: 65536}),
				"CheckCompShareResourceCapacity": tt.capacity,
			}}
			result, err := NewEngine(executor, func(string, map[string]any) bool {
				t.Fatal("an unverified target must not reach confirmation")
				return true
			}, nil).Run(context.Background(), ResizeInstanceDef(), map[string]any{"UHostId": "uhost-test", "Gpu": float64(2)})

			require.NoError(t, err)
			require.False(t, result.Success)
			assert.Equal(t, "检查目标规格库存", result.StoppedAt)
			assert.Contains(t, result.Message, tt.message)
			_, priced := findExecutorCall(executor.calls, "GetCompShareInstanceUpgradePrice")
			assert.False(t, priced)
		})
	}
}
