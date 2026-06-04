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
			"Zone":       "cn-sh2-02",
			"GpuType":    "4090",
			"GPU":        float64(1),
			"ChargeType": "Dynamic",
		},
	}}
}

// --- CreateDisk tests ---

func TestCreateDisk_HappyPath(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance":    stoppedInstanceResult(),
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

	var createCall executorCall
	for _, c := range executor.calls {
		if c.action == "CreateAndAttachCompshareDisk" {
			createCall = c
		}
	}
	assert.Equal(t, "SSDDataDisk", createCall.args["DiskType"], "must use SSDDataDisk")
	assert.Equal(t, "Dynamic", createCall.args["ChargeType"], "must default to Dynamic")
	assert.Equal(t, "test-gpu-data", createCall.args["Name"], "Name should be instance name + -data")
	assert.NotEmpty(t, createCall.args["Name"], "Name must be set")
	assert.Contains(t, createCall.args["Name"], "data", "Name should contain 'data'")
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

func diskResizeInstanceResult() map[string]any {
	return map[string]any{"UHostSet": []any{
		map[string]any{
			"UHostId":    "uhost-test",
			"Name":       "test-gpu",
			"State":      "Stopped",
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
		"DescribeCompShareInstance": stoppedInstanceResult(),
		"DescribeCompShareImages":   {"ImageSet": []any{}},
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
	assert.Equal(t, "查询目标镜像", result.StoppedAt)

	hasConfirm := false
	for _, ev := range *events {
		if ev.Type == StepConfirm {
			hasConfirm = true
		}
	}
	assert.False(t, hasConfirm, "confirmation should NOT be reached when target image is missing")
}
