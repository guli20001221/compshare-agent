package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stoppedInstanceResult() map[string]any {
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
		},
	}}
}

func podStoppedInstanceResult() map[string]any {
	return map[string]any{"UHostSet": []any{
		map[string]any{
			"UHostId":      "cpod-test",
			"Name":         "pod-gpu",
			"State":        "Stopped",
			"Region":       "cn-pod",
			"Zone":         "cn-pod-01",
			"ZoneId":       float64(9001),
			"RegionId":     float64(3001),
			"InstanceType": "Container",
			"GpuType":      "4090",
			"GPU":          float64(1),
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
	_, priced := findExecutorCall(executor.calls, "GetCompShareInstancePrice")
	assert.True(t, priced, "create disk workflow must price the new data disk before confirmation")

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

func TestCreateDisk_ContainerInstanceBlockedBeforeConfirm(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":    containerStoppedInstanceResult(),
		"CreateAndAttachCompshareDisk": {"UDiskId": "udisk-new"},
	}}

	def := CreateDiskDef()
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("Container 实例不支持普通数据盘时不应进入确认")
		return true
	}, nil)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-container-test",
		"Size":    float64(100),
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "数据盘")
	_, created := findExecutorCall(executor.calls, "CreateAndAttachCompshareDisk")
	assert.False(t, created)
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
		"DescribeCompShareInstance":        stoppedInstanceResult(),
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
		"DescribeCompShareInstance":        stoppedInstanceResult(),
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
				"UHostId":  "uhost-test",
				"Name":     "test-gpu",
				"State":    "Stopped",
				"Region":   "cn-sh2",
				"Zone":     "cn-sh2-02",
				"ZoneId":   float64(9001),
				"RegionId": float64(3001),
			},
		}},
		"GetCompShareInstanceUpgradePrice": {"Price": float64(1.5)},
		"ResizeCompShareInstance":          {"RetCode": 0},
	}}
	def := ResizeInstanceDef()
	eng := NewEngine(executor, func(action string, args map[string]any) bool { return true }, nil)

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
	resizeCall, ok := findExecutorCall(executor.calls, "ResizeCompShareInstance")
	require.True(t, ok)
	assert.Equal(t, uint32(9001), resizeCall.args["zone_id"])
	assert.Equal(t, uint32(3001), resizeCall.args["az_group"])
}

func TestResize_PassesParamsToAPI(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":        stoppedInstanceResult(),
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

func TestReinstall_PasswordBase64Encoded(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":  stoppedInstanceResult(),
		"DescribeCompShareImages":    {"ImageSet": []any{map[string]any{"CompShareImageId": "img-001", "Name": "Ubuntu"}}},
		"ReinstallCompShareInstance": {"RetCode": 0},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }
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
	assert.Equal(t, "TXlQYXNzMTIzIQ==", reinstallCall.args["Password"], "password must be base64-encoded")
	assert.Equal(t, "Password", reinstallCall.args["LoginMode"])
}

func TestReinstall_CommunityImageLookupAccepted(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": stoppedInstanceResult(),
		"DescribeCompShareImages":   {"ImageSet": []any{}},
		"DescribeCommunityImages": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "comm-img-001", "Name": "DeepSeek-R1:32b", "Container": "False"},
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
	_, reinstalled := findExecutorCall(executor.calls, "ReinstallCompShareInstance")
	assert.True(t, reinstalled)
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

func TestReinstall_ContainerRejectsNonContainerImageBeforeConfirm(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": containerStoppedInstanceResult(),
		"DescribeCompShareImages": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-001", "Name": "Ubuntu-nvidia 22.04", "Container": "False"},
		}},
	}}

	def := ReinstallInstanceDef()
	eng := NewEngine(executor, func(action string, args map[string]any) bool {
		t.Fatal("Container 重装非容器镜像时不应进入确认")
		return true
	}, nil)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":          "uhost-container-test",
		"CompShareImageId": "img-001",
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "容器镜像")
	_, reinstalled := findExecutorCall(executor.calls, "ReinstallCompShareInstance")
	assert.False(t, reinstalled)
}

func TestReinstall_RunningInstanceBlocked(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{"UHostId": "uhost-test", "State": "Running"},
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
