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
