package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func scheduleAfter(minutes float64) map[string]any {
	return map[string]any{"mode": "after_minutes", "minutes": minutes}
}

func TestResolveShutdownTime_AfterMinutes(t *testing.T) {
	withFixedNow(t)

	params := map[string]any{"Schedule": scheduleAfter(60)}
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
	params := map[string]any{"Schedule": map[string]any{"mode": "absolute", "at": target.Format(time.RFC3339)}}
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
	params := map[string]any{"Schedule": map[string]any{"mode": "absolute", "at": plain}}
	unix, display, err := resolveShutdownTime(params)

	assert.NoError(t, err)
	assert.Equal(t, targetBeijing.Unix(), unix)
	assert.Contains(t, display, "北京时间")
}

func TestResolveShutdownTime_IgnoresInactiveModeFields(t *testing.T) {
	withFixedNow(t)

	tests := []struct {
		name     string
		schedule map[string]any
		want     time.Time
	}{
		{
			name: "calendar time ignores stale relative and absolute fields",
			schedule: map[string]any{
				"mode": "today", "local_time": "23:00", "minutes": float64(5), "at": "",
			},
			want: time.Date(2026, 4, 16, 23, 0, 0, 0, shanghaiLoc),
		},
		{
			name: "absolute time ignores stale relative and calendar fields",
			schedule: map[string]any{
				"mode": "absolute", "at": "2026-04-17T00:00:00+08:00", "minutes": float64(5), "local_time": "",
			},
			want: time.Date(2026, 4, 17, 0, 0, 0, 0, shanghaiLoc),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			unix, _, err := resolveShutdownTime(map[string]any{"Schedule": tc.schedule})
			require.NoError(t, err)
			assert.Equal(t, tc.want.Unix(), unix)
		})
	}
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

	params := map[string]any{"Schedule": scheduleAfter(3)}
	_, _, err := resolveShutdownTime(params)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "5 分钟")
}

func TestResolveShutdownTime_AfterMinutes_Fractional_Error(t *testing.T) {
	withFixedNow(t)

	params := map[string]any{"Schedule": scheduleAfter(30.5)}
	_, _, err := resolveShutdownTime(params)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "正整数")
}

func TestResolveShutdownTime_TooSoon_Error(t *testing.T) {
	withFixedNow(t)

	// 2 minutes from now — less than the 5-minute minimum.
	target := fixedNow.Add(2 * time.Minute)
	params := map[string]any{"Schedule": map[string]any{"mode": "absolute", "at": target.Format(time.RFC3339)}}
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
				"Region":     "cn-bj2",
				"GpuType":    "4090",
				"GPU":        float64(1),
				"ChargeType": "Dynamic",
			},
		}},
		"UpdateCompShareStopScheduler": {"RetCode": 0},
	}}
}

type schedulerReadbackExecutor struct {
	calls     []executorCall
	stopTime  int64
	cancelled bool
	describes int
}

func (e *schedulerReadbackExecutor) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	e.calls = append(e.calls, executorCall{action: action, args: args})
	switch action {
	case "UpdateCompShareStopScheduler":
		e.stopTime, _ = args["SchedulerStopTime"].(int64)
		return map[string]any{"RetCode": 0}, nil
	case "DeleteCompShareStopScheduler":
		e.cancelled = true
		return map[string]any{"RetCode": 0}, nil
	case "DescribeCompShareInstance":
		e.describes++
		row := map[string]any{
			"UHostId": "uhost-xxx", "Name": "my-gpu", "State": "Running",
			"Zone": "cn-bj2-04", "Region": "cn-bj2", "GpuType": "4090",
			"GPU": float64(1), "ChargeType": "Dynamic",
		}
		if !e.cancelled && e.stopTime > 0 {
			row["SchedulerStopTime"] = e.stopTime
		}
		return map[string]any{"UHostSet": []any{row}}, nil
	default:
		return map[string]any{"RetCode": 0}, nil
	}
}

func TestSetStopScheduler_HappyPath(t *testing.T) {
	withFixedNow(t)

	executor := schedulerMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := SetStopSchedulerDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-xxx",
		"Schedule": scheduleAfter(60),
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Len(t, result.Steps, 6)

	// Verify UpdateCompShareStopScheduler was called with correct args.
	assert.Len(t, executor.calls, 3)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
	assert.Equal(t, "UpdateCompShareStopScheduler", executor.calls[1].action)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[2].action)

	callArgs := executor.calls[1].args
	assert.Equal(t, "cn-bj2-04", callArgs["Zone"])
	assert.Equal(t, "cn-bj2", callArgs["Region"])
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
		"UHostId":  "uhost-nonexistent",
		"Schedule": scheduleAfter(60),
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "查询实例", result.StoppedAt)
	assert.Contains(t, result.Message, "未找到")
}

func TestSetStopScheduler_InitializingInstanceCanSchedule(t *testing.T) {
	withFixedNow(t)

	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-xxx",
				"State":      "Initializing",
				"Zone":       "cn-bj2-04",
				"Region":     "cn-bj2",
				"ChargeType": "Dynamic",
			},
		}},
		"UpdateCompShareStopScheduler": {"RetCode": 0},
	}}
	onStep, _ := collectEvents()

	def := SetStopSchedulerDef()
	eng := NewEngine(executor, func(string, map[string]any) bool { return true }, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-xxx",
		"Schedule": scheduleAfter(60),
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	require.Len(t, executor.calls, 3)
	assert.Equal(t, "UpdateCompShareStopScheduler", executor.calls[1].action)
}

func TestSetStopScheduler_SpotRejected(t *testing.T) {
	withFixedNow(t)

	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-xxx",
				"State":      "Running",
				"ChargeType": "Preemptive",
				"IsSpot":     true,
			},
		}},
	}}
	onStep, _ := collectEvents()

	def := SetStopSchedulerDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-xxx",
		"Schedule": scheduleAfter(60),
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
		"UHostId":  "uhost-xxx",
		"Schedule": scheduleAfter(60),
	})

	assert.NoError(t, err)
	assert.NotNil(t, capturedArgs)

	shutdownTime, ok := capturedArgs["shutdownTime"].(string)
	assert.True(t, ok, "shutdownTime should be a string")
	assert.Contains(t, shutdownTime, "北京时间")
}

func TestSetStopScheduler_RelativeTimeStartsWhenConfirmationIsAccepted(t *testing.T) {
	withFixedNow(t)
	fixed := fixedNow
	executor := schedulerMockExecutor()
	var confirmed string
	confirmFn := func(_ string, args map[string]any) bool {
		confirmed, _ = args["shutdownTime"].(string)
		nowFunc = func() time.Time { return fixed.Add(30 * time.Minute) }
		return true
	}
	onStep, _ := collectEvents()

	result, err := NewEngine(executor, confirmFn, onStep).Run(
		context.Background(),
		SetStopSchedulerDef(),
		map[string]any{"UHostId": "uhost-xxx", "Schedule": scheduleAfter(60)},
	)

	require.NoError(t, err)
	require.True(t, result.Success)
	require.Contains(t, confirmed, fixed.Add(time.Hour).In(shanghaiLoc).Format("2006-01-02 15:04"))
	require.Len(t, executor.calls, 3)
	assert.Equal(t, fixed.Add(90*time.Minute).Unix(), executor.calls[1].args["SchedulerStopTime"],
		"the sealed rule is 60 minutes after approval, not the preview's stale timestamp")
}

func TestSetStopScheduler_ExactMinimumKeepsTransportAllowance(t *testing.T) {
	withFixedNow(t)
	executor := schedulerMockExecutor()
	result, err := NewEngine(executor, func(string, map[string]any) bool { return true }, nil).Run(
		context.Background(), SetStopSchedulerDef(),
		map[string]any{"UHostId": "uhost-xxx", "Schedule": scheduleAfter(5)},
	)
	require.NoError(t, err)
	require.True(t, result.Success, result.Message)
	got := executor.calls[1].args["SchedulerStopTime"].(int64)
	assert.Equal(t, fixedNow.Add(5*time.Minute+shutdownTransportAllowance).Unix(), got)
}

func TestSetStopScheduler_RevalidatesAbsoluteTimeAfterConfirmation(t *testing.T) {
	withFixedNow(t)
	fixed := fixedNow
	executor := schedulerMockExecutor()
	result, err := NewEngine(executor, func(string, map[string]any) bool {
		nowFunc = func() time.Time { return fixed.Add(8 * time.Minute) }
		return true
	}, nil).Run(context.Background(), SetStopSchedulerDef(), map[string]any{
		"UHostId":  "uhost-xxx",
		"Schedule": map[string]any{"mode": "absolute", "at": fixed.Add(10 * time.Minute).Format(time.RFC3339)},
	})
	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, shutdownFinalStepName, result.StoppedAt)
	assert.Len(t, executor.calls, 1, "an absolute time that expired while awaiting approval must not be written")
}

func TestSetStopScheduler_ReadbackVerifiesRequestedTime(t *testing.T) {
	withFixedNow(t)
	executor := &schedulerReadbackExecutor{}
	result, err := NewEngine(executor, func(string, map[string]any) bool { return true }, nil).Run(
		context.Background(), SetStopSchedulerDef(),
		map[string]any{"UHostId": "uhost-xxx", "Schedule": scheduleAfter(60)},
	)
	require.NoError(t, err)
	require.True(t, result.Success, result.Message)
	assert.Equal(t, true, result.Data["Verified"])
	assert.Equal(t, executor.stopTime, result.Data["ObservedStopTime"])
}

func TestSetStopScheduler_ConfirmDenied(t *testing.T) {
	withFixedNow(t)

	executor := schedulerMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return false }
	onStep, _ := collectEvents()

	def := SetStopSchedulerDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-xxx",
		"Schedule": scheduleAfter(60),
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
		"UHostId":  "uhost-xxx",
		"Schedule": scheduleAfter(3),
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "5 分钟")
}

func TestSetStopScheduler_MissingZoneRejectedBeforeMutation(t *testing.T) {
	withFixedNow(t)

	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-no-zone",
				"Name":       "no-zone",
				"State":      "Running",
				"GpuType":    "4090",
				"GPU":        float64(1),
				"ChargeType": "Dynamic",
			},
		}},
		"UpdateCompShareStopScheduler": {"RetCode": 0},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := SetStopSchedulerDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-no-zone",
		"Schedule": scheduleAfter(60),
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "可用区")
	assert.Len(t, executor.calls, 1)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
}

func TestSetStopScheduler_MissingRegionIsNotDerivedFromZone(t *testing.T) {
	withFixedNow(t)

	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-no-region",
				"Name":       "no-region",
				"State":      "Running",
				"Zone":       "cn-sh2-02",
				"GpuType":    "4090",
				"GPU":        float64(1),
				"ChargeType": "Dynamic",
			},
		}},
		"UpdateCompShareStopScheduler": {"RetCode": 0},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := SetStopSchedulerDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":  "uhost-no-region",
		"Schedule": scheduleAfter(60),
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "真实地域")
	assert.Len(t, executor.calls, 1)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
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
				"Region":     "cn-bj2",
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
	assert.Len(t, result.Steps, 4)
	assert.Equal(t, true, result.Data["Verified"])
	assert.Equal(t, int64(0), result.Data["ObservedStopTime"])

	// Verify DeleteCompShareStopScheduler was called with UHostId and Region.
	assert.Len(t, executor.calls, 3)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
	assert.Equal(t, "DeleteCompShareStopScheduler", executor.calls[1].action)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[2].action)
	assert.Equal(t, "uhost-xxx", executor.calls[1].args["UHostId"])
	assert.Contains(t, executor.calls[1].args, "Region", "DeleteCompShareStopScheduler must include Region")
}

func TestCancelStopScheduler_ReadbackKeepsUnverifiedWhenTimeRemains(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{map[string]any{
			"UHostId": "uhost-xxx", "Name": "my-gpu", "State": "Running",
			"Zone": "cn-bj2-04", "Region": "cn-bj2", "SchedulerStopTime": float64(1778420000),
		}}},
		"DeleteCompShareStopScheduler": {"RetCode": 0},
	}}
	result, err := NewEngine(executor, func(string, map[string]any) bool { return true }, nil).Run(
		context.Background(), CancelStopSchedulerDef(), map[string]any{"UHostId": "uhost-xxx"},
	)
	require.NoError(t, err)
	require.True(t, result.Success)
	assert.Equal(t, false, result.Data["Verified"])
	assert.Equal(t, int64(1778420000), result.Data["ObservedStopTime"])
}

func TestCancelStopScheduler_UsesReturnedInstanceRegion(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-sh",
				"Name":       "sh-gpu",
				"State":      "Running",
				"Zone":       "cn-sh2-02",
				"Region":     "cn-sh2",
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
	assert.Equal(t, "cn-sh2", deleteCall.args["Region"], "Region must come from the queried instance")
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
				"Region":     "cn-bj2",
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
	assert.Len(t, result.Steps, 4)

	// Stopped instance should still allow cancellation of residual scheduler tasks.
	assert.Len(t, executor.calls, 3)
	assert.Equal(t, "DeleteCompShareStopScheduler", executor.calls[1].action)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[2].action)
}

func TestCancelStopScheduler_MissingZoneRejectedBeforeMutation(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-no-zone",
				"Name":       "no-zone",
				"State":      "Stopped",
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
		"UHostId": "uhost-no-zone",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "可用区")
	assert.Len(t, executor.calls, 1)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
}

func TestCancelStopScheduler_MissingRegionIsNotDerivedFromZone(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-no-region",
				"Name":       "no-region",
				"State":      "Stopped",
				"Zone":       "cn-sh2-02",
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
		"UHostId": "uhost-no-region",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Contains(t, result.Message, "真实地域")
	assert.Len(t, executor.calls, 1)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
}
