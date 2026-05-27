package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// fixedNow is a deterministic "current time" used across scheduler tests.
// 2026-04-16 14:00:00 UTC  ==  2026-04-16 22:00:00 Beijing
var fixedNow = time.Date(2026, 4, 16, 14, 0, 0, 0, time.UTC)

// withFixedNow overrides nowFunc for the duration of a test and restores it
// on cleanup.
func withFixedNow(t *testing.T) {
	t.Helper()
	orig := nowFunc
	nowFunc = func() time.Time { return fixedNow }
	t.Cleanup(func() { nowFunc = orig })
}

func TestResolveShutdownTime_AfterMinutes(t *testing.T) {
	withFixedNow(t)

	params := map[string]any{"AfterMinutes": float64(60)}
	unix, display, err := resolveShutdownTime(params)

	assert.NoError(t, err)
	// Timestamp should be now + 3600 seconds.
	assert.Equal(t, fixedNow.Add(60*time.Minute).Unix(), unix)
	// Display must mention Beijing time.
	assert.Contains(t, display, "北京时间")
}

func TestResolveShutdownTime_ShutdownAt_WithTimezone(t *testing.T) {
	withFixedNow(t)

	// 2 hours from fixedNow, expressed in RFC3339.
	target := fixedNow.Add(2 * time.Hour)
	params := map[string]any{"ShutdownAt": target.Format(time.RFC3339)}
	unix, display, err := resolveShutdownTime(params)

	assert.NoError(t, err)
	assert.Equal(t, target.Unix(), unix)
	assert.Contains(t, display, "北京时间")
	assert.Contains(t, display, "2 小时")
}

func TestResolveShutdownTime_ShutdownAt_NoTimezone(t *testing.T) {
	withFixedNow(t)

	// 2 hours from fixedNow in Beijing time: 2026-04-17 00:00
	targetBeijing := fixedNow.Add(2 * time.Hour).In(shanghaiLoc)
	plain := targetBeijing.Format("2006-01-02 15:04")
	params := map[string]any{"ShutdownAt": plain}
	unix, display, err := resolveShutdownTime(params)

	assert.NoError(t, err)
	assert.Equal(t, targetBeijing.Unix(), unix)
	assert.Contains(t, display, "北京时间")
}

func TestResolveShutdownTime_BothProvided_Error(t *testing.T) {
	withFixedNow(t)

	params := map[string]any{
		"AfterMinutes": float64(30),
		"ShutdownAt":   "2026-04-17T00:00:00+08:00",
	}
	_, _, err := resolveShutdownTime(params)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "不能同时指定")
}

func TestResolveShutdownTime_NeitherProvided_Error(t *testing.T) {
	withFixedNow(t)

	params := map[string]any{}
	_, _, err := resolveShutdownTime(params)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "请指定关机时间")
}

func TestResolveShutdownTime_AfterMinutes_TooSmall(t *testing.T) {
	withFixedNow(t)

	params := map[string]any{"AfterMinutes": float64(3)}
	_, _, err := resolveShutdownTime(params)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "5 分钟")
}

func TestResolveShutdownTime_AfterMinutes_Fractional_Error(t *testing.T) {
	withFixedNow(t)

	params := map[string]any{"AfterMinutes": 30.5}
	_, _, err := resolveShutdownTime(params)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "正整数")
}

func TestResolveShutdownTime_TooSoon_Error(t *testing.T) {
	withFixedNow(t)

	// 2 minutes from now — less than the 5-minute minimum.
	target := fixedNow.Add(2 * time.Minute)
	params := map[string]any{"ShutdownAt": target.Format(time.RFC3339)}
	_, _, err := resolveShutdownTime(params)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "5 分钟")
}

// ---------------------------------------------------------------------------
// SetStopScheduler workflow tests
// ---------------------------------------------------------------------------

// schedulerMockExecutor returns a mock where the instance is Running/Dynamic
// and UpdateCompShareStopScheduler succeeds.
func schedulerMockExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-xxx",
				"Name":       "my-gpu",
				"State":      "Running",
				"Zone":       "cn-bj2-04",
				"GpuType":    "4090",
				"GPU":        float64(1),
				"ChargeType": "Dynamic",
			},
		}},
		"UpdateCompShareStopScheduler": {"RetCode": 0},
	}}
}

func TestSetStopScheduler_HappyPath(t *testing.T) {
	withFixedNow(t)

	executor := schedulerMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := SetStopSchedulerDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":      "uhost-xxx",
		"AfterMinutes": float64(60),
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Len(t, result.Steps, 3)

	// Verify UpdateCompShareStopScheduler was called with correct args.
	assert.Len(t, executor.calls, 2)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
	assert.Equal(t, "UpdateCompShareStopScheduler", executor.calls[1].action)

	callArgs := executor.calls[1].args
	assert.Equal(t, "cn-bj2-04", callArgs["Zone"])
	assert.Equal(t, "uhost-xxx", callArgs["UHostId"])

	// SchedulerStopTime must be an int64 in the future.
	stopTime, ok := callArgs["SchedulerStopTime"].(int64)
	assert.True(t, ok, "SchedulerStopTime should be int64")
	assert.Greater(t, stopTime, fixedNow.Unix())
}

func TestSetStopScheduler_NotFound(t *testing.T) {
	withFixedNow(t)

	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}},
	}}
	onStep, _ := collectEvents()

	def := SetStopSchedulerDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":      "uhost-nonexistent",
		"AfterMinutes": float64(60),
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "查询实例", result.StoppedAt)
	assert.Contains(t, result.Message, "未找到")
}

func TestSetStopScheduler_NotRunning(t *testing.T) {
	withFixedNow(t)

	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-xxx",
				"State":      "Stopped",
				"ChargeType": "Dynamic",
			},
		}},
	}}
	onStep, _ := collectEvents()

	def := SetStopSchedulerDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":      "uhost-xxx",
		"AfterMinutes": float64(60),
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "查询实例", result.StoppedAt)
	assert.Contains(t, result.Message, "未运行")
}

func TestSetStopScheduler_SpotRejected(t *testing.T) {
	withFixedNow(t)

	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-xxx",
				"State":      "Running",
				"ChargeType": "Spot",
			},
		}},
	}}
	onStep, _ := collectEvents()

	def := SetStopSchedulerDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":      "uhost-xxx",
		"AfterMinutes": float64(60),
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "查询实例", result.StoppedAt)
	assert.Contains(t, result.Message, "抢占式")
}

func TestSetStopScheduler_ConfirmShowsTime(t *testing.T) {
	withFixedNow(t)

	executor := schedulerMockExecutor()

	var capturedArgs map[string]any
	confirmFn := func(action string, args map[string]any) bool {
		capturedArgs = args
		return false // don't need to proceed further
	}
	onStep, _ := collectEvents()

	def := SetStopSchedulerDef()
	eng := NewEngine(executor, confirmFn, onStep)
	_, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":      "uhost-xxx",
		"AfterMinutes": float64(60),
	})

	assert.NoError(t, err)
	assert.NotNil(t, capturedArgs)

	shutdownTime, ok := capturedArgs["shutdownTime"].(string)
	assert.True(t, ok, "shutdownTime should be a string")
	assert.Contains(t, shutdownTime, "北京时间")
}

func TestSetStopScheduler_ConfirmDenied(t *testing.T) {
	withFixedNow(t)

	executor := schedulerMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return false }
	onStep, _ := collectEvents()

	def := SetStopSchedulerDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":      "uhost-xxx",
		"AfterMinutes": float64(60),
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "确认设置", result.StoppedAt)
}

func TestSetStopScheduler_BadTime(t *testing.T) {
	withFixedNow(t)

	executor := schedulerMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := SetStopSchedulerDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":      "uhost-xxx",
		"AfterMinutes": float64(3),
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "5 分钟")
}

// ---------------------------------------------------------------------------
// CancelStopScheduler workflow tests
// ---------------------------------------------------------------------------

// cancelSchedulerMockExecutor returns a mock where the instance is
// Running/Dynamic and DeleteCompShareStopScheduler succeeds.
func cancelSchedulerMockExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-xxx",
				"Name":       "my-gpu",
				"State":      "Running",
				"Zone":       "cn-bj2-04",
				"GpuType":    "4090",
				"GPU":        float64(1),
				"ChargeType": "Dynamic",
			},
		}},
		"DeleteCompShareStopScheduler": {"RetCode": 0},
	}}
}

func TestCancelStopScheduler_HappyPath(t *testing.T) {
	executor := cancelSchedulerMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := CancelStopSchedulerDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-xxx",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Len(t, result.Steps, 3)

	// Verify DeleteCompShareStopScheduler was called with UHostId and Region.
	assert.Len(t, executor.calls, 2)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
	assert.Equal(t, "DeleteCompShareStopScheduler", executor.calls[1].action)
	assert.Equal(t, "uhost-xxx", executor.calls[1].args["UHostId"])
	assert.Contains(t, executor.calls[1].args, "Region", "DeleteCompShareStopScheduler must include Region")
}

func TestCancelStopScheduler_UsesInstanceRegion(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-sh",
				"Name":       "sh-gpu",
				"State":      "Running",
				"Zone":       "cn-sh2-02",
				"GpuType":    "H20",
				"GPU":        float64(1),
				"ChargeType": "Dynamic",
			},
		}},
		"DeleteCompShareStopScheduler": {"RetCode": 0},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := CancelStopSchedulerDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-sh",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	deleteCall := executor.calls[1]
	assert.Equal(t, "cn-sh2", deleteCall.args["Region"], "Region must be derived from instance Zone cn-sh2-02, not default")
}

func TestCancelStopScheduler_NotFound(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}},
	}}
	onStep, _ := collectEvents()

	def := CancelStopSchedulerDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-nonexistent",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "查询实例", result.StoppedAt)
	assert.Contains(t, result.Message, "未找到")
}

func TestCancelStopScheduler_ConfirmDenied(t *testing.T) {
	executor := cancelSchedulerMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return false }
	onStep, _ := collectEvents()

	def := CancelStopSchedulerDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-xxx",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "确认取消", result.StoppedAt)
}

func TestCancelStopScheduler_StoppedInstance_Allowed(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-xxx",
				"Name":       "my-gpu",
				"State":      "Stopped",
				"Zone":       "cn-bj2-04",
				"GpuType":    "4090",
				"GPU":        float64(1),
				"ChargeType": "Dynamic",
			},
		}},
		"DeleteCompShareStopScheduler": {"RetCode": 0},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := CancelStopSchedulerDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-xxx",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Len(t, result.Steps, 3)

	// Stopped instance should still allow cancellation of residual scheduler tasks.
	assert.Len(t, executor.calls, 2)
	assert.Equal(t, "DeleteCompShareStopScheduler", executor.calls[1].action)
}
