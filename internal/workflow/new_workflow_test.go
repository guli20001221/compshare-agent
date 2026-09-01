package workflow

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/deployment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stoppedInstanceResult() map[string]any {
	return map[string]any{"UHostSet": []any{
		map[string]any{
			"UHostId":          "uhost-test",
			"Name":             "test-gpu",
			"State":            "Stopped",
			"Region":           "cn-sh2",
			"Zone":             "cn-sh2-02",
			"GpuType":          "4090",
			"MachineType":      "G",
			"CpuPlatform":      "Auto",
			"CompShareImageId": "img-001",
			"GPU":              float64(1),
			"CPU":              float64(16),
			"Memory":           float64(65536),
			"ChargeType":       "Dynamic",
			"IsSpot":           false,
			"DiskSet": []any{
				map[string]any{
					"Type":     "Boot",
					"DiskType": "CLOUD_SSD",
					"Size":     float64(100),
				},
			},
		},
	}}
}

func podStoppedInstanceResult() map[string]any {
	return map[string]any{"UHostSet": []any{
		map[string]any{
			"UHostId":          "cpod-test",
			"Name":             "pod-gpu",
			"State":            "Stopped",
			"Region":           "cn-pod",
			"Zone":             "cn-pod-01",
			"ZoneId":           float64(9001),
			"RegionId":         float64(3001),
			"InstanceType":     "Container",
			"GpuType":          "4090",
			"MachineType":      "G",
			"CpuPlatform":      "Auto",
			"CompShareImageId": "img-pod",
			"GPU":              float64(1),
			"CPU":              float64(10),
			"Memory":           float64(65536),
			"ChargeType":       "Dynamic",
			"DiskSet": []any{
				map[string]any{
					"DiskId":   "cvolume-boot",
					"Name":     "pod-sys",
					"Type":     "Boot",
					"DiskType": "CLOUD_SSD",
					"Size":     float64(60),
				},
			},
		},
	}}
}

func containerStoppedInstanceResult() map[string]any {
	return map[string]any{"UHostSet": []any{
		map[string]any{
			"UHostId":      "uhost-container-test",
			"Name":         "container-gpu",
			"State":        "Stopped",
			"Region":       "cn-pod",
			"Zone":         "cn-pod-01",
			"InstanceType": "Container",
			"GpuType":      "4090",
			"GPU":          float64(1),
			"CPU":          float64(10),
			"Memory":       float64(65536),
			"ChargeType":   "Dynamic",
			"DiskSet": []any{
				map[string]any{
					"DiskId":   "cvolume-boot",
					"Name":     "pod-sys",
					"Type":     "Boot",
					"DiskType": "CLOUD_SSD",
					"Size":     float64(60),
				},
			},
		},
	}}
}

func resizeInstanceTypesResult(gpuType, zone string, gpuCount float64, specs ...specCandidate) map[string]any {
	collections := make([]any, 0, len(specs))
	for _, spec := range specs {
		collections = append(collections, map[string]any{
			"Cpu":    spec.CPU,
			"Memory": []any{spec.MemoryMB / 1024},
		})
	}
	return map[string]any{"AvailableInstanceTypes": []any{
		map[string]any{
			"Name": gpuType,
			"Zone": zone,
			"MachineSizes": []any{map[string]any{
				"Gpu":        gpuCount,
				"Collection": collections,
			}},
		},
	}}
}

func resizeSupportZonesResult(zone, region string, zoneID, regionID float64, isPod bool) map[string]any {
	return map[string]any{"ZoneInfo": []any{map[string]any{
		"Zone":     zone,
		"Region":   region,
		"ZoneId":   zoneID,
		"RegionId": regionID,
		"IsPod":    isPod,
	}}}
}

func resizeCapacityResult(gpu, cpu, memoryGB float64, enough bool) map[string]any {
	return map[string]any{"Specs": []any{map[string]any{
		"Gpu":            gpu,
		"Cpu":            cpu,
		"Mem":            memoryGB,
		"ResourceEnough": enough,
	}}}
}

// --- CreateDisk tests ---

func TestCreateDisk_HappyPath(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":    stoppedInstanceResult(),
		"GetCompShareInstancePrice":    {"PriceDetails": []any{map[string]any{"Disks": float64(0.8)}}},
		"CreateAndAttachCompshareDisk": {"UDiskId": "udisk-new"},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := CreateDiskDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-test",
		"Size":    float64(100),
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	describeCall, described := findExecutorCall(executor.calls, "DescribeCompShareInstance")
	require.True(t, described)
	assert.NotContains(t, describeCall.args, "WithoutGpu", "data-disk creation must query the selected instance normally")
	priceCall, priced := findExecutorCall(executor.calls, "GetCompShareInstancePrice")
	assert.True(t, priced, "create disk workflow must price the new data disk before confirmation")
	assert.Equal(t, "4090", priceCall.args["GpuType"], "data-disk pricing must include the source instance GPU type; upstream rejects missing Gpu/GpuType")
	assert.Equal(t, float64(1), priceCall.args["Gpu"], "data-disk pricing must include the source instance GPU count")
	assert.Equal(t, float64(16), priceCall.args["Cpu"], "data-disk pricing must include the source instance CPU")
	assert.Equal(t, float64(65536), priceCall.args["Memory"], "data-disk pricing must include the source instance memory in MB")
	assert.Equal(t, "cn-sh2", priceCall.args["Region"], "data-disk pricing must use the source instance region")
	assert.Equal(t, "cn-sh2-02", priceCall.args["Zone"], "data-disk pricing must use the source instance zone")
	priceDisks, ok := priceCall.args["Disks"].([]any)
	require.True(t, ok, "data-disk pricing must pass Disks")
	require.Len(t, priceDisks, 1)
	priceDisk, ok := priceDisks[0].(map[string]any)
	require.True(t, ok, "data-disk pricing disk must be an object")
	assert.Equal(t, "CLOUD_SSD", priceDisk["Type"], "price API expects UDisk type; upstream converts it to SSDDataDisk for billing")
	assert.Equal(t, false, priceDisk["IsBoot"])

	var createCall executorCall
	for _, c := range executor.calls {
		if c.action == "CreateAndAttachCompshareDisk" {
			createCall = c
		}
	}
	assert.Equal(t, "SSDDataDisk", createCall.args["DiskType"], "must use SSDDataDisk")
	assert.Equal(t, "Postpay", createCall.args["ChargeType"], "按量 = Postpay (deprecated Dynamic retired, #246)")
	assert.Equal(t, "test-gpu-data", createCall.args["Name"], "Name should be instance name + -data")
	assert.NotEmpty(t, createCall.args["Name"], "Name must be set")
	assert.Contains(t, createCall.args["Name"], "data", "Name should contain 'data'")
}

func TestCreateDisk_UsesSourceConfigForNoGpuInstancePrice(t *testing.T) {
	noGPU := stoppedInstanceResult()["UHostSet"].([]any)[0].(map[string]any)
	noGPU["GPU"] = float64(0)
	noGPU["CPU"] = float64(8)
	noGPU["Memory"] = float64(16384)
	noGPU["SrcInstanceConfig"] = map[string]any{
		"Cpu":    float64(10),
		"Memory": float64(65536),
		"Gpu":    float64(1),
	}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{noGPU}},
		"GetCompShareInstancePrice": {"PriceDetails": []any{map[string]any{"Disks": float64(0.8)}}},
	}}
	onStep, _ := collectEvents()

	def := CreateDiskDef()
	eng := NewEngine(executor, func(action string, args map[string]any) bool { return false }, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-test",
		"Size":    float64(200),
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "确认创建数据盘", result.StoppedAt)
	describeCall, described := findExecutorCall(executor.calls, "DescribeCompShareInstance")
	require.True(t, described)
	assert.NotContains(t, describeCall.args, "WithoutGpu", "DescribeCompShareInstance must not switch the query into no-GPU mode")
	priceCall, priced := findExecutorCall(executor.calls, "GetCompShareInstancePrice")
	require.True(t, priced)
	assert.Equal(t, "4090", priceCall.args["GpuType"])
	assert.Equal(t, float64(1), priceCall.args["Gpu"])
	assert.Equal(t, float64(10), priceCall.args["Cpu"])
	assert.Equal(t, float64(65536), priceCall.args["Memory"])
}

func TestCreateDisk_BlocksNoGpuInstanceWithoutSourceConfigBeforePrice(t *testing.T) {
	noGPU := stoppedInstanceResult()["UHostSet"].([]any)[0].(map[string]any)
	noGPU["GPU"] = float64(0)
	noGPU["CPU"] = float64(8)
	noGPU["Memory"] = float64(16384)
	delete(noGPU, "SrcInstanceConfig")
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{noGPU}},
		"GetCompShareInstancePrice": {"PriceDetails": []any{map[string]any{"Disks": float64(0.8)}}},
	}}
	onStep, events := collectEvents()

	def := CreateDiskDef()
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("缺少无卡实例原规格时不应进入确认")
		return true
	}, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-test",
		"Size":    float64(200),
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "无卡")
	assert.Contains(t, result.Message, "原带卡规格")
	_, priced := findExecutorCall(executor.calls, "GetCompShareInstancePrice")
	assert.False(t, priced, "缺少原规格时不能调用价格接口")
	for _, ev := range *events {
		assert.NotEqual(t, StepConfirm, ev.Type)
	}
}

func TestCreateDisk_BlocksNoGpuInstanceWithIncompleteSourceConfigBeforePrice(t *testing.T) {
	noGPU := stoppedInstanceResult()["UHostSet"].([]any)[0].(map[string]any)
	noGPU["GPU"] = float64(0)
	noGPU["CPU"] = float64(8)
	noGPU["Memory"] = float64(16384)
	noGPU["SrcInstanceConfig"] = map[string]any{
		"Gpu": float64(1),
	}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{noGPU}},
		"GetCompShareInstancePrice": {"PriceDetails": []any{map[string]any{"Disks": float64(0.8)}}},
	}}
	onStep, events := collectEvents()

	def := CreateDiskDef()
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("原带卡规格不完整时不应进入确认")
		return true
	}, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-test",
		"Size":    float64(200),
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "原带卡规格不完整")
	_, priced := findExecutorCall(executor.calls, "GetCompShareInstancePrice")
	assert.False(t, priced, "原规格不完整时不能混用当前无卡规格继续询价")
	for _, ev := range *events {
		assert.NotEqual(t, StepConfirm, ev.Type)
	}
}

func TestCreateDisk_UsesRequestedInstanceWhenDescribeReturnsExtraRows(t *testing.T) {
	pod := podStoppedInstanceResult()["UHostSet"].([]any)[0]
	normal := stoppedInstanceResult()["UHostSet"].([]any)[0]
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{pod, normal}},
		"GetCompShareInstancePrice": {"PriceDetails": []any{map[string]any{"Disks": float64(0.8)}}},
	}}
	confirmFn := func(action string, args map[string]any) bool { return false }
	onStep, _ := collectEvents()

	def := CreateDiskDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-test",
		"Size":    float64(100),
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "确认创建数据盘", result.StoppedAt)
	assert.NotContains(t, result.Message, "Pod/容器")
	priceCall, priced := findExecutorCall(executor.calls, "GetCompShareInstancePrice")
	require.True(t, priced, "workflow should continue to price the requested normal instance")
	assert.Equal(t, "cn-sh2-02", priceCall.args["Zone"])
}

func TestCreateDisk_BlocksBeforeConfirmWhenPriceMissing(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": stoppedInstanceResult(),
		"GetCompShareInstancePrice": {},
	}}
	onStep, events := collectEvents()

	def := CreateDiskDef()
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("价格缺失时不应进入确认")
		return true
	}, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-test",
		"Size":    float64(100),
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "未获取到价格")
	for _, ev := range *events {
		assert.NotEqual(t, "waiting", ev.Status, "confirmation should not wait when price is missing")
	}
	_, created := findExecutorCall(executor.calls, "CreateAndAttachCompshareDisk")
	assert.False(t, created)
}

func TestCreateDisk_MissingSizeBlockedBeforeConfirm(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":    stoppedInstanceResult(),
		"CreateAndAttachCompshareDisk": {"UDiskId": "udisk-new"},
	}}
	onStep, events := collectEvents()

	def := CreateDiskDef()
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		return true
	}, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-test",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success, "missing Size must not create a disk")
	assert.Contains(t, result.Message, "磁盘大小")

	hasConfirm := false
	for _, ev := range *events {
		if ev.Type == StepConfirm {
			hasConfirm = true
		}
	}
	assert.False(t, hasConfirm, "confirmation should NOT be reached when disk size is missing")

	for _, c := range executor.calls {
		assert.NotEqual(t, "CreateAndAttachCompshareDisk", c.action, "API must not be called without Size")
	}
}

func TestCreateDisk_PodInstanceBlockedBeforeConfirm(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":    podStoppedInstanceResult(),
		"CreateAndAttachCompshareDisk": {"UDiskId": "udisk-new"},
	}}
	onStep, events := collectEvents()

	def := CreateDiskDef()
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("Pod 实例不支持普通数据盘时不应进入确认")
		return true
	}, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "cpod-test",
		"Size":    float64(100),
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "数据盘")

	for _, ev := range *events {
		assert.NotEqual(t, StepConfirm, ev.Type, "confirmation should not be reached for Pod data disk create")
	}
	for _, c := range executor.calls {
		assert.NotEqual(t, "CreateAndAttachCompshareDisk", c.action, "Pod data disk create API must not be called")
	}
}

func TestCreateDisk_ContainerUHostAllowedToConfirm(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":    containerStoppedInstanceResult(),
		"DescribeCompShareSupportZone": cfsSupportZone("cn-pod-01", "cn-pod", "容器一区", 9001, 3001, true),
		"GetCompShareInstancePrice":    {"PriceDetails": []any{map[string]any{"Disks": float64(0.8)}}},
		"CreateAndAttachCompshareDisk": {"UDiskId": "udisk-new"},
	}}

	def := CreateDiskDef()
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		return false
	}, nil)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-container-test",
		"Size":    float64(100),
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "确认创建数据盘", result.StoppedAt)
	assert.NotContains(t, result.Message, "Pod")
	priceCall, priced := findExecutorCall(executor.calls, "GetCompShareInstancePrice")
	require.True(t, priced)
	assert.Equal(t, "4090", priceCall.args["GpuType"])
	assert.Equal(t, float64(1), priceCall.args["Gpu"])
	assert.Equal(t, float64(10), priceCall.args["Cpu"])
	assert.Equal(t, float64(65536), priceCall.args["Memory"])
}

func diskResizeInstanceResult() map[string]any {
	return map[string]any{"UHostSet": []any{
		map[string]any{
			"UHostId":    "uhost-test",
			"Name":       "test-gpu",
			"State":      "Stopped",
			"Region":     "cn-sh2",
			"Zone":       "cn-sh2-02",
			"GpuType":    "4090",
			"GPU":        float64(1),
			"ChargeType": "Dynamic",
			"DiskSet": []any{
				map[string]any{
					"DiskId":   "udisk-boot",
					"Name":     "sys",
					"Type":     "Boot",
					"DiskType": "CLOUD_SSD",
					"Size":     float64(60),
				},
				map[string]any{
					"DiskId":   "udisk-data",
					"Name":     "data",
					"Type":     "Data",
					"DiskType": "SSDDataDisk",
					"Size":     float64(100),
				},
			},
		},
	}}
}

// --- ResizeDisk tests ---

func TestResizeDisk_SystemDiskHappyPath(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":            diskResizeInstanceResult(),
		"CheckCompShareResizeAttachedDisk":     {"RetCode": 0},
		"GetCompShareAttachedDiskUpgradePrice": {"Price": float64(2.5)},
		"ResizeCompShareDisk":                  {"RetCode": 0},
	}}
	var confirmArgs map[string]any
	confirmFn := func(action string, args map[string]any) bool {
		confirmArgs = args
		return true
	}
	onStep, _ := collectEvents()

	def := ResizeDiskDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-test",
		"DiskType": "Boot",
		"Size":     float64(120),
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	require.NotNil(t, confirmArgs)
	assert.Equal(t, "udisk-boot", confirmArgs["disk_id"])
	assert.Equal(t, "Boot", confirmArgs["disk_role"])
	assert.Equal(t, float64(60), confirmArgs["current_size_gb"])
	assert.Equal(t, float64(120), confirmArgs["target_size_gb"])
	assert.Equal(t, float64(2.5), confirmArgs["price_delta"])

	var priceCall executorCall
	var checkCall executorCall
	var resizeCall executorCall
	for _, c := range executor.calls {
		switch c.action {
		case "CheckCompShareResizeAttachedDisk":
			checkCall = c
		case "GetCompShareAttachedDiskUpgradePrice":
			priceCall = c
		case "ResizeCompShareDisk":
			resizeCall = c
		}
	}
	assert.Equal(t, "uhost-test", checkCall.args["UHostId"])
	assert.Equal(t, "udisk-boot", checkCall.args["DiskId"])
	assert.Equal(t, float64(120), checkCall.args["DiskSpace"])
	assert.Equal(t, "cn-sh2-02", checkCall.args["Zone"])
	assert.Equal(t, "cn-sh2", checkCall.args["Region"])
	assert.Equal(t, "uhost-test", priceCall.args["UHostId"])
	assert.Equal(t, "udisk-boot", priceCall.args["DiskId"])
	assert.Equal(t, float64(120), priceCall.args["DiskSpace"])
	assert.Equal(t, "cn-sh2-02", priceCall.args["Zone"])
	assert.Equal(t, "cn-sh2", priceCall.args["Region"])
	assert.Equal(t, "uhost-test", resizeCall.args["UHostId"])
	assert.Equal(t, "udisk-boot", resizeCall.args["UDiskId"])
	assert.Equal(t, float64(120), resizeCall.args["Size"])
}

func TestResizeDisk_PodSystemDiskUsesResizeInstance(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":            podStoppedInstanceResult(),
		"CheckCompShareResizeAttachedDisk":     {"RetCode": 0},
		"GetCompShareAttachedDiskUpgradePrice": {"Price": float64(2.5)},
		"ResizeCompShareInstance":              {"RetCode": 0},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := ResizeDiskDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "cpod-test",
		"DiskType": "Boot",
		"Size":     float64(120),
	})

	require.NoError(t, err)
	assert.True(t, result.Success)

	resizeCall, ok := findExecutorCall(executor.calls, "ResizeCompShareInstance")
	require.True(t, ok, "Pod disk resize must call ResizeCompShareInstance")
	assert.Equal(t, "cpod-test", resizeCall.args["UHostId"])
	assert.Equal(t, "cvolume-boot", resizeCall.args["DiskId"])
	assert.Equal(t, float64(120), resizeCall.args["DiskSpace"])
	assert.Equal(t, "cn-pod-01", resizeCall.args["Zone"])
	assert.Equal(t, "cn-pod", resizeCall.args["Region"])
	_, oldAPICalled := findExecutorCall(executor.calls, "ResizeCompShareDisk")
	assert.False(t, oldAPICalled, "Pod disk resize must not call ResizeCompShareDisk")
}

func TestResizeDisk_MissingPriceBlockedBeforeConfirm(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":            diskResizeInstanceResult(),
		"CheckCompShareResizeAttachedDisk":     {"RetCode": 0},
		"GetCompShareAttachedDiskUpgradePrice": {"RetCode": 0},
		"ResizeCompShareDisk":                  {"RetCode": 0},
	}}
	onStep, events := collectEvents()
	def := ResizeDiskDef()
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("missing disk resize price must be blocked before confirmation")
		return true
	}, onStep)

	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-test",
		"DiskType": "Boot",
		"Size":     float64(120),
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "未获取到价格")
	for _, ev := range *events {
		assert.False(t, ev.Type == StepConfirm && ev.Status == "waiting")
	}
	_, resized := findExecutorCall(executor.calls, "ResizeCompShareDisk")
	assert.False(t, resized)
}

func TestResizeDisk_MissingInstanceLocationBlockedBeforePrice(t *testing.T) {
	instance := diskResizeInstanceResult()
	host := instance["UHostSet"].([]any)[0].(map[string]any)
	delete(host, "Region")
	delete(host, "Zone")
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": instance,
	}}
	def := ResizeDiskDef()
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("missing instance location must be blocked before confirmation")
		return true
	}, nil)

	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-test",
		"DiskType": "Boot",
		"Size":     float64(120),
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "真实可用区")
	_, checked := findExecutorCall(executor.calls, "CheckCompShareResizeAttachedDisk")
	assert.False(t, checked)
	_, priced := findExecutorCall(executor.calls, "GetCompShareAttachedDiskUpgradePrice")
	assert.False(t, priced)
}

func TestResizeDisk_PodMissingInternalPlacementBlockedBeforePrice(t *testing.T) {
	instance := podStoppedInstanceResult()
	host := instance["UHostSet"].([]any)[0].(map[string]any)
	delete(host, "ZoneId")
	delete(host, "RegionId")
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": instance,
	}}
	def := ResizeDiskDef()
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("Pod disk resize without internal placement must be blocked before confirmation")
		return true
	}, nil)

	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "cpod-test",
		"DiskType": "Boot",
		"Size":     float64(120),
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "内部可用区编号")
	_, checked := findExecutorCall(executor.calls, "CheckCompShareResizeAttachedDisk")
	assert.False(t, checked)
	_, priced := findExecutorCall(executor.calls, "GetCompShareAttachedDiskUpgradePrice")
	assert.False(t, priced)
}

func TestResizeDisk_PodSystemDiskCarriesInternalPlacement(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": map[string]any{"UHostSet": []any{
			map[string]any{
				"UHostId":      "cpod-test",
				"Name":         "pod-gpu",
				"State":        "Stopped",
				"Region":       "cn-pod",
				"Zone":         "cn-pod-01",
				"InstanceType": "Container",
				"DiskSet": []any{
					map[string]any{"DiskId": "cvolume-boot", "Name": "pod-sys", "Type": "Boot", "Size": float64(60)},
				},
			},
		}},
		"DescribeCompShareSupportZone": {"ZoneInfo": []any{
			map[string]any{
				"Zone":     "cn-pod-01",
				"Region":   "cn-pod",
				"ZoneId":   float64(9001),
				"RegionId": float64(3001),
				"IsPod":    true,
			},
		}},
		"CheckCompShareResizeAttachedDisk":     {"RetCode": 0},
		"GetCompShareAttachedDiskUpgradePrice": {"Price": float64(2.5)},
		"ResizeCompShareInstance":              {"RetCode": 0},
	}}
	def := ResizeDiskDef()
	eng := NewEngine(executor, func(action string, args map[string]any) bool { return true }, nil)

	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "cpod-test",
		"DiskType": "Boot",
		"Size":     float64(120),
	})

	require.NoError(t, err)
	assert.True(t, result.Success)
	priceCall, ok := findExecutorCall(executor.calls, "GetCompShareAttachedDiskUpgradePrice")
	require.True(t, ok)
	assert.Equal(t, uint32(9001), priceCall.args["zone_id"])
	assert.Equal(t, uint32(3001), priceCall.args["az_group"])
	resizeCall, ok := findExecutorCall(executor.calls, "ResizeCompShareInstance")
	require.True(t, ok)
	assert.Equal(t, uint32(9001), resizeCall.args["zone_id"])
	assert.Equal(t, uint32(3001), resizeCall.args["az_group"])
}

func TestResizeDisk_PodInternalPlacementFallsBackToInstanceFields(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":            podStoppedInstanceResult(),
		"DescribeCompShareSupportZone":         {"ZoneInfo": []any{}},
		"CheckCompShareResizeAttachedDisk":     {"RetCode": 0},
		"GetCompShareAttachedDiskUpgradePrice": {"Price": float64(2.5)},
		"ResizeCompShareInstance":              {"RetCode": 0},
	}}
	def := ResizeDiskDef()
	eng := NewEngine(executor, func(action string, args map[string]any) bool { return true }, nil)

	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "cpod-test",
		"DiskType": "Boot",
		"Size":     float64(120),
	})

	require.NoError(t, err)
	assert.True(t, result.Success)
	resizeCall, ok := findExecutorCall(executor.calls, "ResizeCompShareInstance")
	require.True(t, ok)
	assert.Equal(t, uint32(9001), resizeCall.args["zone_id"])
	assert.Equal(t, uint32(3001), resizeCall.args["az_group"])
}

func TestResizeDisk_BlocksWhenTargetNotLargerThanCurrent(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": diskResizeInstanceResult(),
	}}
	onStep, events := collectEvents()

	def := ResizeDiskDef()
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		return true
	}, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-test",
		"DiskType": "Boot",
		"Size":     float64(60),
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "目标容量")

	for _, ev := range *events {
		assert.NotEqual(t, StepConfirm, ev.Type, "confirmation should not be reached")
	}
	for _, c := range executor.calls {
		assert.NotEqual(t, "ResizeCompShareDisk", c.action, "resize API must not be called")
	}
}

func TestResizeDisk_DataDiskRequiresDiskIDWhenMultipleDataDisks(t *testing.T) {
	instance := diskResizeInstanceResult()
	host := instance["UHostSet"].([]any)[0].(map[string]any)
	host["DiskSet"] = append(host["DiskSet"].([]any), map[string]any{
		"DiskId":   "udisk-data-2",
		"Name":     "data-2",
		"Type":     "Data",
		"DiskType": "SSDDataDisk",
		"Size":     float64(200),
	})
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": instance,
	}}

	def := ResizeDiskDef()
	eng := NewEngine(executor, nil, nil)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-test",
		"DiskType": "Data",
		"Size":     float64(300),
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "DiskId")
}

// --- Resize tests ---

func TestResize_EmptyParams_BlockedBeforeConfirm(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": stoppedInstanceResult(),
	}}
	onStep, events := collectEvents()

	def := ResizeInstanceDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-test",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success, "should fail when no Cpu/Gpu/Memory specified")
	assert.Equal(t, "查询实例", result.StoppedAt)

	hasConfirm := false
	for _, ev := range *events {
		if ev.Type == StepConfirm {
			hasConfirm = true
		}
	}
	assert.False(t, hasConfirm, "confirmation should NOT be reached when params are missing")
}

func TestResize_IncludesPriceInConfirm(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":    stoppedInstanceResult(),
		"DescribeCompShareSupportZone": resizeSupportZonesResult("cn-sh2-02", "cn-sh2", 2002, 1002, false),
		"DescribeAvailableCompShareInstanceTypes": resizeInstanceTypesResult("4090", "cn-sh2-02", 2,
			specCandidate{CPU: 16, MemoryMB: 65536},
		),
		"CheckCompShareResourceCapacity":   resizeCapacityResult(2, 16, 64, true),
		"GetCompShareInstanceUpgradePrice": {"Price": float64(1.5), "OriginalPrice": float64(2.0)},
		"ResizeCompShareInstance":          {"RetCode": 0},
	}}
	var confirmArgs map[string]any
	confirmFn := func(action string, args map[string]any) bool {
		confirmArgs = args
		return true
	}
	onStep, _ := collectEvents()

	def := ResizeInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-test",
		"Gpu":     float64(2),
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	require.NotNil(t, confirmArgs)
	assert.Equal(t, float64(1.5), confirmArgs["price_delta"], "confirm should show price delta")
	assert.Equal(t, float64(2), confirmArgs["target_gpu"])

	priceCall, ok := findExecutorCall(executor.calls, "GetCompShareInstanceUpgradePrice")
	require.True(t, ok)
	assert.Equal(t, "cn-sh2", priceCall.args["Region"])
	assert.Equal(t, "cn-sh2-02", priceCall.args["Zone"])
}

func TestResize_MissingPriceBlockedBeforeConfirm(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":    stoppedInstanceResult(),
		"DescribeCompShareSupportZone": resizeSupportZonesResult("cn-sh2-02", "cn-sh2", 2002, 1002, false),
		"DescribeAvailableCompShareInstanceTypes": resizeInstanceTypesResult("4090", "cn-sh2-02", 2,
			specCandidate{CPU: 16, MemoryMB: 65536},
		),
		"CheckCompShareResourceCapacity":   resizeCapacityResult(2, 16, 64, true),
		"GetCompShareInstanceUpgradePrice": {"RetCode": 0},
		"ResizeCompShareInstance":          {"RetCode": 0},
	}}
	onStep, events := collectEvents()
	def := ResizeInstanceDef()
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("missing resize price must be blocked before confirmation")
		return true
	}, onStep)

	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-test",
		"Gpu":     float64(2),
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "未获取到价格")
	for _, ev := range *events {
		assert.False(t, ev.Type == StepConfirm && ev.Status == "waiting")
	}
	_, resized := findExecutorCall(executor.calls, "ResizeCompShareInstance")
	assert.False(t, resized)
}

func TestResize_UnsupportedCpuMemoryBlockedBeforeConfirm(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": stoppedInstanceResult(),
		"DescribeAvailableCompShareInstanceTypes": resizeInstanceTypesResult("4090", "cn-sh2-02", 1,
			specCandidate{CPU: 16, MemoryMB: 65536},
			specCandidate{CPU: 32, MemoryMB: 131072},
		),
		"GetCompShareInstanceUpgradePrice": {"Price": float64(0)},
		"ResizeCompShareInstance":          {"RetCode": 0},
	}}
	def := ResizeInstanceDef()
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("unsupported resize target must be blocked before confirmation")
		return true
	}, nil)

	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-test",
		"Cpu":     float64(16),
		"Memory":  float64(131072),
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "不支持")
	_, confirmed := findExecutorCall(executor.calls, "ResizeCompShareInstance")
	assert.False(t, confirmed)
	_, priced := findExecutorCall(executor.calls, "GetCompShareInstanceUpgradePrice")
	assert.False(t, priced, "unsupported target should fail before pricing and confirmation")
}

func TestResize_NoEffectiveChangeBlockedBeforePrice(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": stoppedInstanceResult(),
	}}
	def := ResizeInstanceDef()
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("no-op resize must be blocked before confirmation")
		return true
	}, nil)

	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-test",
		"Gpu":     float64(1),
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "目标配置与当前配置一致")
	_, priced := findExecutorCall(executor.calls, "GetCompShareInstanceUpgradePrice")
	assert.False(t, priced)
	_, resized := findExecutorCall(executor.calls, "ResizeCompShareInstance")
	assert.False(t, resized)
}

func TestResize_MissingInstanceLocationBlockedBeforePrice(t *testing.T) {
	instance := stoppedInstanceResult()
	host := instance["UHostSet"].([]any)[0].(map[string]any)
	delete(host, "Region")
	delete(host, "Zone")
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": instance,
	}}
	def := ResizeInstanceDef()
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("missing instance location must be blocked before confirmation")
		return true
	}, nil)

	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-test",
		"Gpu":     float64(2),
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "真实可用区")
	_, priced := findExecutorCall(executor.calls, "GetCompShareInstanceUpgradePrice")
	assert.False(t, priced)
}

func TestResize_CarriesInternalPlacement(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": map[string]any{"UHostSet": []any{
			map[string]any{
				"UHostId":          "uhost-test",
				"Name":             "test-gpu",
				"State":            "Stopped",
				"Region":           "cn-sh2",
				"Zone":             "cn-sh2-02",
				"GpuType":          "4090",
				"MachineType":      "G",
				"CpuPlatform":      "Auto",
				"CompShareImageId": "img-001",
				"GPU":              float64(1),
				"CPU":              float64(16),
				"Memory":           float64(65536),
				"ChargeType":       "Dynamic",
				"ZoneId":           float64(9001),
				"RegionId":         float64(3001),
				"DiskSet": []any{map[string]any{
					"Type": "Boot", "DiskType": "CLOUD_SSD", "Size": float64(100),
				}},
			},
		}},
		"DescribeCompShareSupportZone": resizeSupportZonesResult("cn-sh2-02", "cn-sh2", 9001, 3001, false),
		"DescribeAvailableCompShareInstanceTypes": resizeInstanceTypesResult("4090", "cn-sh2-02", 2,
			specCandidate{CPU: 16, MemoryMB: 65536},
		),
		"CheckCompShareResourceCapacity":   resizeCapacityResult(2, 16, 64, true),
		"GetCompShareInstanceUpgradePrice": {"Price": float64(1.5)},
		"ResizeCompShareInstance":          {"RetCode": 0},
	}}
	def := ResizeInstanceDef()
	var confirmed map[string]any
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		confirmed = args
		return true
	}, nil)

	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-test",
		"Gpu":     float64(2),
	})

	require.NoError(t, err)
	assert.True(t, result.Success)
	priceCall, ok := findExecutorCall(executor.calls, "GetCompShareInstanceUpgradePrice")
	require.True(t, ok)
	assert.Equal(t, uint32(9001), priceCall.args["zone_id"])
	assert.Equal(t, uint32(3001), priceCall.args["az_group"])
	assert.Equal(t, float64(16), priceCall.args["CPU"], "sparse GPU-only request must quote the complete target")
	assert.Equal(t, float64(2), priceCall.args["GPU"])
	assert.Equal(t, float64(65536), priceCall.args["Memory"])
	require.NotNil(t, confirmed)
	assert.Equal(t, float64(16), confirmed["target_cpu"])
	assert.Equal(t, float64(2), confirmed["target_gpu"])
	assert.Equal(t, float64(65536), confirmed["target_memory"])
	resizeCall, ok := findExecutorCall(executor.calls, "ResizeCompShareInstance")
	require.True(t, ok)
	assert.Equal(t, uint32(9001), resizeCall.args["zone_id"])
	assert.Equal(t, uint32(3001), resizeCall.args["az_group"])
	assert.Equal(t, float64(16), resizeCall.args["Cpu"])
	assert.Equal(t, float64(2), resizeCall.args["Gpu"])
	assert.Equal(t, float64(65536), resizeCall.args["Memory"])
}

func TestResize_PassesParamsToAPI(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":    stoppedInstanceResult(),
		"DescribeCompShareSupportZone": resizeSupportZonesResult("cn-sh2-02", "cn-sh2", 2002, 1002, false),
		"DescribeAvailableCompShareInstanceTypes": resizeInstanceTypesResult("4090", "cn-sh2-02", 2,
			specCandidate{CPU: 32, MemoryMB: 131072},
		),
		"CheckCompShareResourceCapacity":   resizeCapacityResult(2, 32, 128, true),
		"GetCompShareInstanceUpgradePrice": {"Price": float64(0)},
		"ResizeCompShareInstance":          {"RetCode": 0},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := ResizeInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	_, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-test",
		"Cpu":     float64(32),
		"Gpu":     float64(2),
		"Memory":  float64(131072),
	})

	assert.NoError(t, err)
	var resizeCall executorCall
	for _, c := range executor.calls {
		if c.action == "ResizeCompShareInstance" {
			resizeCall = c
		}
	}
	assert.Equal(t, float64(32), resizeCall.args["Cpu"])
	assert.Equal(t, float64(2), resizeCall.args["Gpu"])
	assert.Equal(t, float64(131072), resizeCall.args["Memory"])
	assert.Equal(t, "uhost-test", resizeCall.args["UHostId"])
	assert.NotEmpty(t, resizeCall.args["Region"])
}

// --- Reinstall tests ---

func TestReinstall_ShowsImageNameInConfirm(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": stoppedInstanceResult(),
		"DescribeCompShareImages": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-001", "Name": "Ubuntu-nvidia 22.04"},
		}},
		"ReinstallCompShareInstance": {"RetCode": 0},
	}}
	var confirmArgs map[string]any
	confirmFn := func(action string, args map[string]any) bool {
		confirmArgs = args
		return true
	}
	onStep, _ := collectEvents()

	def := ReinstallInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":          "uhost-test",
		"CompShareImageId": "img-001",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	require.NotNil(t, confirmArgs)
	assert.Equal(t, "Ubuntu-nvidia 22.04", confirmArgs["target_image_name"])
	assert.Equal(t, "img-001", confirmArgs["target_image_id"])
	assert.Contains(t, confirmArgs["warning"].(string), "系统盘")
}

func TestReinstall_DoesNotOfferIgnoredPasswordContract(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":  stoppedInstanceResult(),
		"DescribeCompShareImages":    {"ImageSet": []any{map[string]any{"CompShareImageId": "img-001", "Name": "Ubuntu"}}},
		"ReinstallCompShareInstance": {"RetCode": 0},
	}}
	var confirmed map[string]any
	confirmFn := func(action string, args map[string]any) bool {
		confirmed = args
		return true
	}
	onStep, _ := collectEvents()

	def := ReinstallInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	_, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":          "uhost-test",
		"CompShareImageId": "img-001",
		"Password":         "MyPass123!",
	})

	assert.NoError(t, err)
	var reinstallCall executorCall
	for _, c := range executor.calls {
		if c.action == "ReinstallCompShareInstance" {
			reinstallCall = c
		}
	}
	assert.NotContains(t, reinstallCall.args, "Password", "upstream reinstall ignores the request password")
	assert.NotContains(t, reinstallCall.args, "LoginMode", "upstream reinstall owns credential handling")
	assert.Contains(t, confirmed["credential_handling"], "本次重装不接受新密码")
}

func TestReinstall_CommunityImageLookupAccepted(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": stoppedInstanceResult(),
		"DescribeCompShareImages":   {"ImageSet": []any{}},
		"DescribeCommunityImages": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "comm-img-001", "Name": "DeepSeek-R1:32b", "Container": "False", "Price": float64(12.5)},
		}},
		"DescribeCompShareCustomImages":  {"ImageSet": []any{}},
		"DescribeCompShareSharingImages": {"ImageSet": []any{}},
		"ReinstallCompShareInstance":     {"RetCode": 0},
	}}
	var confirmArgs map[string]any
	confirmFn := func(action string, args map[string]any) bool {
		confirmArgs = args
		return true
	}
	onStep, _ := collectEvents()

	def := ReinstallInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":          "uhost-test",
		"CompShareImageId": "comm-img-001",
	})

	require.NoError(t, err)
	assert.True(t, result.Success)
	require.NotNil(t, confirmArgs)
	assert.Equal(t, "DeepSeek-R1:32b", confirmArgs["target_image_name"])
	assert.Equal(t, "community", confirmArgs["target_image_source"])
	assert.Contains(t, confirmArgs["target_image_price"], "12.50")
	_, reinstalled := findExecutorCall(executor.calls, "ReinstallCompShareInstance")
	assert.True(t, reinstalled)
}

func TestReinstallFailureDraftCapturesThePreConfirmImageFromTheInstanceRow(t *testing.T) {
	instance := stoppedInstanceResult()
	host := instance["UHostSet"].([]any)[0].(map[string]any)
	host["CompShareImageId"] = "img-current"
	host["CompShareImageName"] = "Ubuntu 22.04"
	executor := &mockExecutor{
		results: map[string]map[string]any{
			"DescribeCompShareInstance": instance,
			"DescribeCompShareImages": {"ImageSet": []any{map[string]any{
				"CompShareImageId": "img-current", "Name": "Ubuntu 22.04", "Container": "False",
			}}},
		},
		failOn: "ReinstallCompShareInstance",
	}
	result, err := NewEngine(executor, func(string, map[string]any) bool { return true }, nil).Run(
		context.Background(), ReinstallInstanceDef(), map[string]any{
			"UHostId": "uhost-test", "CompShareImageId": "img-current", "ImageSource": "platform",
		},
	)
	require.NoError(t, err)
	require.False(t, result.Success)
	require.NotNil(t, result.Failure)
	assert.Equal(t, "img-current", result.Failure.Draft["InitialImageId"])
	assert.Equal(t, "Ubuntu 22.04", result.Failure.Draft["InitialImageName"])
	assert.Equal(t, "img-current", result.Failure.Draft["TargetImageId"])
}

func TestReinstallTenantImageSourcesNeverUsePointReads(t *testing.T) {
	for _, source := range []string{"custom", "sharing"} {
		t.Run(source, func(t *testing.T) {
			args, ok := reinstallImageLookupArgs(NewContext(map[string]any{
				"ImageSource":      source,
				"CompShareImageId": "compshareImage-tenant-visible",
			}), source)
			require.True(t, ok)
			assert.Equal(t, maxCustomImageQueryLimit, args["Limit"])
			assert.NotContains(t, args, "CompShareImageId")
		})
	}
}

func TestReinstallTenantImageOutsideBrowsePageUsesVerifiedTenantSnapshot(t *testing.T) {
	const imageID = "compshareImage-shared-beyond-first-page"
	wfCtx := NewContext(map[string]any{
		"ImageSource":      "sharing",
		"CompShareImageId": imageID,
	})
	wfCtx.StepResults["查询共享目标镜像"] = map[string]any{
		"ImageSet": []any{map[string]any{
			"CompShareImageId": "compshareImage-other",
			"Name":             "另一张共享镜像",
			"Status":           "Available",
		}},
	}
	wfCtx.referenceData = ReferenceData{ImageCatalog: deployment.NewImageCatalogSnapshot(true, []deployment.ImageCatalogEntry{{
		ID: imageID, Name: "租户可见的共享镜像", Source: "sharing", Status: deployment.ImageStatusReviewing,
	}})}

	image, ok := targetReinstallImage(wfCtx)
	require.True(t, ok)
	assert.Equal(t, imageID, image.ID)
	assert.Equal(t, "sharing", image.Source)
}

func TestReinstall_CommunityImageWithoutPriceStopsBeforeConfirmation(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": stoppedInstanceResult(),
		"DescribeCommunityImages": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "comm-img-001", "Name": "DeepSeek-R1:32b", "Container": "False"},
		}},
	}}
	result, err := NewEngine(executor, func(string, map[string]any) bool {
		t.Fatal("a paid community-image reinstall cannot be confirmed without its catalog price")
		return false
	}, nil).Run(context.Background(), ReinstallInstanceDef(), map[string]any{
		"UHostId": "uhost-test", "CompShareImageId": "comm-img-001", "ImageSource": "community",
	})
	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "未返回价格")
	_, reinstalled := findExecutorCall(executor.calls, "ReinstallCompShareInstance")
	assert.False(t, reinstalled)
}

func TestReinstall_PodRejectsNonContainerImageBeforeConfirm(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": podStoppedInstanceResult(),
		"DescribeCompShareImages": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-001", "Name": "Ubuntu-nvidia 22.04", "Container": "False"},
		}},
	}}
	onStep, events := collectEvents()

	def := ReinstallInstanceDef()
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("Pod 重装非容器镜像时不应进入确认")
		return true
	}, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":          "cpod-test",
		"CompShareImageId": "img-001",
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "容器镜像")
	for _, ev := range *events {
		assert.NotEqual(t, "waiting", ev.Status, "confirmation should not wait for user on incompatible Pod image")
	}
	_, reinstalled := findExecutorCall(executor.calls, "ReinstallCompShareInstance")
	assert.False(t, reinstalled)
}

func TestReinstall_RejectsImageThatDoesNotSupportCurrentGPUBeforeConfirm(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": stoppedInstanceResult(),
		"DescribeCompShareImages": {"ImageSet": []any{
			map[string]any{
				"CompShareImageId": "img-5090-only", "Name": "5090 only",
				"Container": "False", "SupportedGpuTypes": []any{"5090"},
			},
		}},
	}}
	eng := NewEngine(executor, func(string, map[string]any) bool {
		t.Fatal("GPU 不兼容的重装不应进入确认")
		return true
	}, nil)

	result, err := eng.Run(context.Background(), ReinstallInstanceDef(), map[string]any{
		"UHostId": "uhost-test", "CompShareImageId": "img-5090-only",
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "不支持当前实例的 GPU 机型 4090")
	_, reinstalled := findExecutorCall(executor.calls, "ReinstallCompShareInstance")
	assert.False(t, reinstalled)
}

func TestReinstall_NoCardInstanceUsesOriginalGPUForCompatibility(t *testing.T) {
	instance := stoppedInstanceResult()
	host := instance["UHostSet"].([]any)[0].(map[string]any)
	host["GpuType"] = ""
	host["SrcInstanceConfig"] = map[string]any{"GpuType": "A800"}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": instance,
		"DescribeCompShareImages": {"ImageSet": []any{
			map[string]any{
				"CompShareImageId": "img-4090-only", "Name": "4090 only",
				"Container": "False", "SupportedGpuTypes": []any{"4090"},
			},
		}},
	}}
	eng := NewEngine(executor, func(string, map[string]any) bool {
		t.Fatal("无卡状态也必须按原带卡规格拒绝不兼容镜像")
		return true
	}, nil)

	result, err := eng.Run(context.Background(), ReinstallInstanceDef(), map[string]any{
		"UHostId": "uhost-test", "CompShareImageId": "img-4090-only",
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "GPU 机型 A800")
}

func TestReinstall_NoCardMachineModeRefusesBeforeConfirmation(t *testing.T) {
	instance := stoppedInstanceResult()
	host := instance["UHostSet"].([]any)[0].(map[string]any)
	host["MachineType"] = "O"
	host["GPU"] = float64(0)
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": instance,
		"DescribeCompShareImages": {"ImageSet": []any{
			map[string]any{
				"CompShareImageId": "img-platform", "Name": "Ubuntu-nvidia 22.04",
				"Container": "False", "SupportedGpuTypes": []any{"4090"},
			},
		}},
	}}
	eng := NewEngine(executor, func(string, map[string]any) bool {
		t.Fatal("无卡模式不应进入重装确认")
		return true
	}, nil)

	result, err := eng.Run(context.Background(), ReinstallInstanceDef(), map[string]any{
		"UHostId": "uhost-test", "CompShareImageId": "img-platform",
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "先恢复带卡运行并关机")
	_, reinstalled := findExecutorCall(executor.calls, "ReinstallCompShareInstance")
	assert.False(t, reinstalled)
}

func TestReinstall_BlocksWhenImageRequiresLargerSystemDisk(t *testing.T) {
	instance := stoppedInstanceResult()
	host := instance["UHostSet"].([]any)[0].(map[string]any)
	host["DiskSet"] = []any{map[string]any{
		"DiskId": "boot-disk",
		"Type":   "Boot",
		"Size":   float64(100),
	}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": instance,
		"DescribeCompShareImages": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-large", "Name": "Large image", "Size": float64(200 * 1024)},
		}},
		"ReinstallCompShareInstance": {"RetCode": 0},
	}}
	onStep, events := collectEvents()

	def := ReinstallInstanceDef()
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("系统盘过小时不应进入确认")
		return true
	}, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":          "uhost-test",
		"CompShareImageId": "img-large",
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "系统盘")
	for _, ev := range *events {
		assert.NotEqual(t, "waiting", ev.Status, "confirmation should not wait when system disk is too small")
	}
	_, reinstalled := findExecutorCall(executor.calls, "ReinstallCompShareInstance")
	assert.False(t, reinstalled)
}

func TestReinstall_StoppedUHostContainerCanReplaceSystemDiskWithHostImage(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":    containerStoppedInstanceResult(),
		"DescribeCompShareSupportZone": cfsSupportZone("cn-pod-01", "cn-pod", "容器一区", 9001, 3001, true),
		"DescribeCompShareImages": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-001", "Name": "Ubuntu-nvidia 22.04", "Container": "False"},
		}},
		"ReinstallCompShareInstance": {"RetCode": 0},
	}}

	var confirmed map[string]any
	def := ReinstallInstanceDef()
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		confirmed = args
		return true
	}, nil)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":          "uhost-container-test",
		"CompShareImageId": "img-001",
	})

	require.NoError(t, err)
	assert.True(t, result.Success)
	require.NotNil(t, confirmed)
	assert.Contains(t, confirmed["warning"], "系统盘")
	_, reinstalled := findExecutorCall(executor.calls, "ReinstallCompShareInstance")
	assert.True(t, reinstalled)
}

func TestReinstall_RunningUHostSystemDiskPathBlocked(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId": "uhost-test", "State": "Running", "Region": "cn-sh2", "Zone": "cn-sh2-02",
				"InstanceType": "Normal", "GpuType": "4090", "GPU": float64(1),
			},
		}},
		"DescribeCompShareImages": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-001", "Name": "Ubuntu", "Container": "False"},
		}},
	}}
	onStep, _ := collectEvents()

	def := ReinstallInstanceDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":          "uhost-test",
		"CompShareImageId": "img-001",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "关机")
}

func TestValidateReinstallStateForPath_MatchesUpstreamMatrix(t *testing.T) {
	instance := func(id, kind, state string) map[string]any {
		return map[string]any{"UHostSet": []any{map[string]any{
			"UHostId": id, "InstanceType": kind, "State": state,
		}}}
	}
	tests := []struct {
		name      string
		result    map[string]any
		container bool
		wantErr   bool
	}{
		{"pod running", instance("cpod-a", "Container", "Running"), true, false},
		{"pod stopping", instance("cpod-a", "Container", "Stopping"), true, false},
		{"pod stopped", instance("cpod-a", "Container", "Stopped"), true, false},
		{"uhost container-to-container running", instance("uhost-a", "Container", "Running"), true, false},
		{"uhost container-to-container stopped", instance("uhost-a", "Container", "Stopped"), true, true},
		{"uhost host disk replacement stopped", instance("uhost-a", "Normal", "Stopped"), false, false},
		{"uhost host disk replacement running", instance("uhost-a", "Normal", "Running"), false, true},
		{"uhost container-to-host stopped", instance("uhost-a", "Container", "Stopped"), false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateReinstallStateForPath(tc.result, reinstallImageInfo{Container: tc.container})
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestReinstall_ZoneImageRestrictionBlocksBeforeConfirmation(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": stoppedInstanceResult(),
		"DescribeCompShareImages": {"ImageSet": []any{map[string]any{
			"CompShareImageId": "img-community", "Name": "community image", "ImageType": "Community", "Container": "False",
		}}},
	}}
	zones := deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{{
		Placement:   deployment.ZonePlacement{Zone: "cn-sh2-02", Region: "cn-sh2", ZoneID: 8200},
		DisplayName: "上海二B", UnsupportedImageTypes: []string{"Community"},
	}})
	result, err := NewEngine(executor, func(string, map[string]any) bool {
		t.Fatal("live zone restriction must reject before confirmation")
		return true
	}, nil).Run(context.Background(), ReinstallInstanceDef(), map[string]any{
		"UHostId": "uhost-test", "CompShareImageId": "img-community",
	}, WithReferenceData(ReferenceData{ZoneCatalog: zones}))
	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "上海二B 当前不支持 Community 类型镜像")
	_, called := findExecutorCall(executor.calls, "ReinstallCompShareInstance")
	assert.False(t, called)
}

func TestReinstall_TargetImageMissingBlockedBeforeConfirm(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":      stoppedInstanceResult(),
		"DescribeCompShareImages":        {"ImageSet": []any{}},
		"DescribeCommunityImages":        {"ImageSet": []any{}},
		"DescribeCompShareCustomImages":  {"ImageSet": []any{}},
		"DescribeCompShareSharingImages": {"ImageSet": []any{}},
	}}
	onStep, events := collectEvents()

	def := ReinstallInstanceDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":          "uhost-test",
		"CompShareImageId": "img-missing",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "确认重装", result.StoppedAt)

	hasConfirm := false
	for _, ev := range *events {
		if ev.Type == StepConfirm && ev.Status == "waiting" {
			hasConfirm = true
		}
	}
	assert.False(t, hasConfirm, "confirmation should NOT be reached when target image is missing")
}
