package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stopMockExecutor returns a mock with results for the StopInstance workflow.
func stopMockExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-xxx",
				"Name":       "my-gpu",
				"State":      "Running",
				"Region":     "cn-wlcb",
				"Zone":       "cn-wlcb-01",
				"GpuType":    "4090",
				"GPU":        float64(1),
				"ChargeType": "Dynamic",
			},
		}},
		"StopCompShareInstance": {"RetCode": 0},
	}}
}

// stoppedMockExecutor returns a mock where the instance is already stopped.
func stoppedMockExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-xxx",
				"Name":       "my-gpu",
				"State":      "Stopped",
				"Region":     "cn-wlcb",
				"Zone":       "cn-wlcb-01",
				"GpuType":    "4090",
				"GPU":        float64(1),
				"ChargeType": "Dynamic",
			},
		}},
	}}
}

// startMockExecutor returns a mock with results for the StartInstance workflow.
func startMockExecutor() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-yyy",
				"Name":       "start-me",
				"State":      "Stopped",
				"Zone":       "cn-bj2-04",
				"Region":     "cn-bj2",
				"GpuType":    "A100",
				"GPU":        float64(2),
				"ChargeType": "Month",
			},
		}},
		"StartCompShareInstance": {"RetCode": 0},
	}}
}

func TestStopInstance_HappyPath(t *testing.T) {
	executor := stopMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := StopInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-xxx",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Len(t, result.Steps, 4)
	for i := range result.Steps {
		assert.Equal(t, def.Steps[i].Name, result.Steps[i].Name)
		assert.Equal(t, "success", result.Steps[i].Status)
	}

	assert.Len(t, executor.calls, 3)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
	assert.Equal(t, "DescribeCompShareSupportZone", executor.calls[1].action)
	assert.Equal(t, "StopCompShareInstance", executor.calls[2].action)
}

func TestStopInstance_ConfirmDenied(t *testing.T) {
	executor := stopMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return false }
	onStep, _ := collectEvents()

	def := StopInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-xxx",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, def.Steps[2].Name, result.StoppedAt)
	assert.Len(t, executor.calls, 2)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
	assert.Equal(t, "DescribeCompShareSupportZone", executor.calls[1].action)
}

func TestStopInstance_ConfirmHasFeeWarning(t *testing.T) {
	executor := stopMockExecutor()

	var capturedArgs map[string]any
	confirmFn := func(action string, args map[string]any) bool {
		capturedArgs = args
		return false
	}
	onStep, _ := collectEvents()

	def := StopInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	_, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-xxx",
	})

	assert.NoError(t, err)
	assert.NotNil(t, capturedArgs)
	warning, ok := capturedArgs["warning"].(string)
	assert.True(t, ok)
	assert.NotEmpty(t, warning)
	assert.Contains(t, warning, "不会取消或退款已购买的计费周期")
	assert.NotContains(t, warning, "实例和 GPU 停止计费")
	assert.NotContains(t, warning, "100GB",
		"the free allowance varies by disk type and region — promising a fixed 100GB is a billing claim we cannot make")
}

func TestStopBillingWarningMatchesChargeContract(t *testing.T) {
	tests := []struct {
		charge string
		want   string
		not    string
	}{
		{charge: "Postpay", want: "结束当前实例/GPU 的运行计费段", not: "不会取消或退款已购买的计费周期"},
		{charge: "Spot", want: "结束当前实例/GPU 的运行计费段", not: "不会取消或退款已购买的计费周期"},
		{charge: "Month", want: "不会取消或退款已购买的计费周期", not: "结束当前实例/GPU 的运行计费段"},
		{charge: "Dynamic", want: "不会取消或退款已购买的计费周期", not: "结束当前实例/GPU 的运行计费段"},
		{charge: "", want: "不能据此确认费用已经停止", not: "结束当前实例/GPU 的运行计费段"},
	}
	for _, tc := range tests {
		t.Run(tc.charge, func(t *testing.T) {
			warning := stopBillingWarning(map[string]any{"ChargeType": tc.charge})
			assert.Contains(t, warning, tc.want)
			assert.NotContains(t, warning, tc.not)
		})
	}
}

func TestStopInstance_AlreadyStopped(t *testing.T) {
	executor := stoppedMockExecutor()
	onStep, _ := collectEvents()

	def := StopInstanceDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-xxx",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, def.Steps[0].Name, result.StoppedAt)
	assert.Len(t, executor.calls, 1)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
}

func TestStopInstance_NotFound(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}},
	}}
	onStep, _ := collectEvents()

	def := StopInstanceDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-nonexistent",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "查询实例", result.StoppedAt)
	assert.Contains(t, result.Message, "未找到")
	// StopCompShareInstance should NOT be called
	assert.Len(t, executor.calls, 1)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
}

func TestStartInstance_HappyPath(t *testing.T) {
	executor := startMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := StartInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-yyy",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Len(t, result.Steps, 3)
	assert.Equal(t, []string{"查询实例", "确认开机", "开机"}, workflowStepNames(result.Steps))
	for i := range result.Steps {
		assert.Equal(t, "success", result.Steps[i].Status)
	}

	assert.Len(t, executor.calls, 2)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
	assert.Equal(t, "StartCompShareInstance", executor.calls[1].action)
	assert.Equal(t, "cn-bj2-04", executor.calls[1].args["Zone"])
}

func TestStartInstance_MissingLocationRejectedBeforeStart(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId": "uhost-yyy",
				"Name":    "start-me",
				"State":   "Stopped",
			},
		}},
		"StartCompShareInstance": {"RetCode": 0},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := StartInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-yyy",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, "确认开机", result.StoppedAt)
	assert.Contains(t, result.Message, "可用区")
	assert.Len(t, executor.calls, 1)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
}

func workflowStepNames(steps []StepSummary) []string {
	out := make([]string, 0, len(steps))
	for _, step := range steps {
		out = append(out, step.Name)
	}
	return out
}

func TestStartInstance_ConfirmDenied(t *testing.T) {
	executor := startMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return false }
	onStep, _ := collectEvents()

	def := StartInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-yyy",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, def.Steps[1].Name, result.StoppedAt)
	assert.Len(t, executor.calls, 1)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
}

func TestStartInstance_ConfirmShowsSummary(t *testing.T) {
	executor := startMockExecutor()

	var capturedArgs map[string]any
	confirmFn := func(action string, args map[string]any) bool {
		capturedArgs = args
		return false
	}
	onStep, _ := collectEvents()

	def := StartInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	_, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-yyy",
	})

	assert.NoError(t, err)
	assert.NotNil(t, capturedArgs)
	assert.Equal(t, "uhost-yyy", capturedArgs["UHostId"])
	assert.Equal(t, "start-me", capturedArgs["Name"])
	assert.Equal(t, "Stopped", capturedArgs["State"])
	assert.Equal(t, "A100", capturedArgs["GpuType"])
	assert.Equal(t, float64(2), capturedArgs["GPU"])
	assert.Equal(t, "Month", capturedArgs["ChargeType"])
}

func TestStartInstance_RunningRejected(t *testing.T) {
	executor := startMockExecutor()
	executor.results["DescribeCompShareInstance"] = map[string]any{
		"UHostSet": []any{
			map[string]any{
				"UHostId": "uhost-yyy",
				"State":   "Running",
			},
		},
	}
	onStep, _ := collectEvents()

	def := StartInstanceDef()
	eng := NewEngine(executor, nil, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-yyy",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.Equal(t, def.Steps[0].Name, result.StoppedAt)
	assert.NotEmpty(t, result.Message)
	assert.Len(t, executor.calls, 1)
	assert.Equal(t, "DescribeCompShareInstance", executor.calls[0].action)
}

// Upstream's StopCompShareInstance request has no Force field and its handler has
// no branch for one, so the old "Spot -> Force:true" was dead code dressed up as a
// contract. Sending it told the reader we knew something about spot stops that we
// did not.
func TestStopInstance_SpotInstanceOmitsForce(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-spot",
				"Name":       "spot-gpu",
				"State":      "Running",
				"Region":     "cn-wlcb",
				"Zone":       "cn-wlcb-01",
				"GpuType":    "4090",
				"GPU":        float64(1),
				"ChargeType": "Spot",
			},
		}},
		"StopCompShareInstance": {"RetCode": 0},
	}}
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := StopInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-spot",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	stopCall := executor.calls[2]
	assert.Equal(t, "StopCompShareInstance", stopCall.action)
	_, hasForce := stopCall.args["Force"]
	assert.False(t, hasForce, "upstream StopCompShareInstance has no Force field; it must not be sent")
}

func TestStopInstance_NonSpotOmitsForce(t *testing.T) {
	executor := stopMockExecutor()
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := StopInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	_, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId": "uhost-xxx",
	})

	assert.NoError(t, err)
	stopCall := executor.calls[2]
	_, hasForce := stopCall.args["Force"]
	assert.False(t, hasForce, "Non-Spot instance stop must not include Force")
}

func TestStartInstance_WithoutGpuSendsSpecOnStart(t *testing.T) {
	executor := startMockExecutor()
	executor.results["DescribeCompShareInstance"] = map[string]any{
		"UHostSet": []any{
			map[string]any{
				"UHostId":                "uhost-yyy",
				"Name":                   "start-me",
				"State":                  "Stopped",
				"Zone":                   "cn-bj2-04",
				"Region":                 "cn-bj2",
				"GpuType":                "4090",
				"GPU":                    float64(1),
				"ChargeType":             "Dynamic",
				"SupportWithoutGpuStart": true,
				"WithoutGpuSpec": map[string]any{
					"Cpu":    float64(2),
					"Memory": float64(4096),
					"Gpu":    float64(0),
				},
			},
		},
	}
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := StartInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":        "uhost-yyy",
		"WithoutGpuSpec": "B",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	// No separate resize call — upstream StartCompShareInstance takes
	// WithoutGpuSpec directly and resizes internally before starting; a raw
	// WithoutGpu boolean on ResizeCompShareInstance is rejected outright.
	assert.Len(t, executor.calls, 2)
	queryCall := executor.calls[0]
	assert.Equal(t, "DescribeCompShareInstance", queryCall.action)
	assert.NotContains(t, queryCall.args, "WithoutGpu", "DescribeCompShareInstance must query the source instance normally")
	startCall := executor.calls[1]
	assert.Equal(t, "StartCompShareInstance", startCall.action)
	assert.NotContains(t, startCall.args, "WithoutGpu", "the deprecated boolean must never be sent")
	assert.Equal(t, "B", startCall.args["WithoutGpuSpec"])
}

func TestStartInstance_CPUOnlyModeMapsToTheUpstreamWireValue(t *testing.T) {
	executor := startMockExecutor()
	executor.results["DescribeCompShareInstance"] = map[string]any{"UHostSet": []any{map[string]any{
		"UHostId": "uhost-yyy", "State": "Stopped", "Zone": "cn-bj2-04", "Region": "cn-bj2",
		"GpuType": "4090", "GPU": float64(1), "ChargeType": "Dynamic", "SupportWithoutGpuStart": true,
	}}}
	result, err := NewEngine(executor, func(string, map[string]any) bool { return true }, nil).Run(
		context.Background(), StartInstanceDef(), map[string]any{
			"UHostId": "uhost-yyy", "StartMode": "cpu_only_8c16g",
		})
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Len(t, executor.calls, 2)
	require.Equal(t, "B", executor.calls[1].args["WithoutGpuSpec"])
	require.NotContains(t, executor.calls[1].args, "StartMode")
}

func TestStartInstance_WithoutGpuShowsInConfirm(t *testing.T) {
	executor := startMockExecutor()
	executor.results["DescribeCompShareInstance"] = map[string]any{
		"UHostSet": []any{
			map[string]any{
				"UHostId":                "uhost-yyy",
				"Name":                   "start-me",
				"State":                  "Stopped",
				"Zone":                   "cn-bj2-04",
				"Region":                 "cn-bj2",
				"GpuType":                "4090",
				"GPU":                    float64(1),
				"CPU":                    float64(16),
				"Memory":                 float64(65536),
				"ChargeType":             "Dynamic",
				"SupportWithoutGpuStart": true,
				"WithoutGpuSpec": map[string]any{
					"Cpu":    float64(2),
					"Memory": float64(4096),
					"Gpu":    float64(0),
				},
			},
		},
	}
	var capturedArgs map[string]any
	confirmFn := func(action string, args map[string]any) bool {
		capturedArgs = args
		return false
	}
	onStep, _ := collectEvents()

	def := StartInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	_, _ = eng.Run(context.Background(), def, map[string]any{
		"UHostId":        "uhost-yyy",
		"WithoutGpuSpec": "B",
	})

	// The card must state the change as a change. The previous shape put the
	// replacement in four unlabelled without_gpu_* keys while leaving the
	// instance's CURRENT GpuType/GPU rows on the card, so the console's most
	// prominent line read "GPU 4090 × 1" on the card that removed the 4090.
	change, ok := capturedArgs["规格变更"].(string)
	require.True(t, ok, "confirm summary must carry the spec change: %v", capturedArgs)
	assert.Contains(t, change, "4090 × 1")
	assert.Contains(t, change, "16核")
	assert.Contains(t, change, "64GB")
	assert.Contains(t, change, "0 GPU")
	assert.Contains(t, change, "8核")
	assert.Contains(t, change, "16GB")
	assert.Contains(t, capturedArgs, "注意")
	assert.NotContains(t, capturedArgs, "GpuType",
		"the current GPU must not stay on a card that takes it away")
	assert.NotContains(t, capturedArgs, "GPU")
}

// A plain start is untouched by the no-GPU card work: it still shows the
// instance's own GPU rows and carries neither of the no-GPU strings.
func TestStartInstance_NormalStartConfirmIsUnchanged(t *testing.T) {
	executor := startMockExecutor()
	executor.results["DescribeCompShareInstance"] = map[string]any{
		"UHostSet": []any{
			map[string]any{
				"UHostId":    "uhost-yyy",
				"Name":       "start-me",
				"State":      "Stopped",
				"Zone":       "cn-bj2-04",
				"Region":     "cn-bj2",
				"GpuType":    "3090",
				"GPU":        float64(1),
				"CPU":        float64(16),
				"Memory":     float64(65536),
				"ChargeType": "Dynamic",
			},
		},
	}
	var capturedArgs map[string]any
	confirmFn := func(_ string, args map[string]any) bool {
		capturedArgs = args
		return false
	}
	onStep, _ := collectEvents()

	_, _ = NewEngine(executor, confirmFn, onStep).Run(context.Background(), StartInstanceDef(), map[string]any{
		"UHostId": "uhost-yyy",
	})

	assert.Equal(t, "3090", capturedArgs["GpuType"])
	assert.Equal(t, float64(1), capturedArgs["GPU"])
	assert.NotContains(t, capturedArgs, "规格变更")
	assert.NotContains(t, capturedArgs, "注意")
}

func TestStartInstance_CurrentNoGPUPlainStartDisclosesHiddenRestore(t *testing.T) {
	executor := startMockExecutor()
	executor.results["DescribeCompShareInstance"] = map[string]any{"UHostSet": []any{
		map[string]any{
			"UHostId": "cpod-yyy", "InstanceType": "Container", "Name": "no-gpu",
			"State": "Stopped", "Zone": "cn-bj2-04", "Region": "cn-bj2",
			"GpuType": "4090", "GPU": float64(0), "CPU": float64(2), "Memory": float64(4096),
			"ChargeType":     "Postpay",
			"WithoutGpuSpec": map[string]any{"Spec": "A", "Cpu": float64(2), "Memory": float64(4096), "Gpu": float64(0)},
		},
	}}
	var captured map[string]any
	result, err := NewEngine(executor, func(_ string, args map[string]any) bool {
		captured = args
		return false
	}, nil).Run(context.Background(), StartInstanceDef(), map[string]any{"UHostId": "cpod-yyy"})
	require.NoError(t, err)
	assert.False(t, result.Success)
	change := captured["规格变更"].(string)
	assert.Contains(t, change, "无卡（0 GPU / 2核 / 4GB）")
	assert.Contains(t, change, "GPU 型号 4090")
	assert.Contains(t, captured["注意"], "恢复存档的原带卡规格")
	assert.Contains(t, captured["注意"], "不能独立完成精确库存和报价预检")
	assert.NotContains(t, captured, "GpuType")
	assert.NotContains(t, captured, "GPU")
}

func TestStartInstance_WithoutGpuUsesDefaultSpecWhenPreviewMissing(t *testing.T) {
	executor := startMockExecutor()
	executor.results["DescribeCompShareInstance"] = map[string]any{
		"UHostSet": []any{
			map[string]any{
				"UHostId":                "uhost-yyy",
				"Name":                   "start-me",
				"State":                  "Stopped",
				"Zone":                   "cn-bj2-04",
				"Region":                 "cn-bj2",
				"GpuType":                "4090",
				"GPU":                    float64(1),
				"ChargeType":             "Dynamic",
				"SupportWithoutGpuStart": true,
				// No WithoutGpuSpec sub-object: this instance has never run
				// in no-GPU mode before, so the live preview is absent — the
				// confirm-card display falls back to the tier-A defaults, but
				// the actual start call must still succeed and target tier A.
			},
		},
	}
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := StartInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":        "uhost-yyy",
		"WithoutGpuSpec": "A",
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Len(t, executor.calls, 2)
	startCall := executor.calls[1]
	assert.Equal(t, "StartCompShareInstance", startCall.action)
	assert.Equal(t, "A", startCall.args["WithoutGpuSpec"])
}

func TestStartInstance_WithoutGpuUnsupportedRejectedBeforeConfirm(t *testing.T) {
	executor := startMockExecutor()
	executor.results["DescribeCompShareInstance"] = map[string]any{
		"UHostSet": []any{
			map[string]any{
				"UHostId":                "uhost-yyy",
				"State":                  "Stopped",
				"Zone":                   "cn-bj2-04",
				"Region":                 "cn-bj2",
				"GpuType":                "H800",
				"GPU":                    float64(1),
				"ChargeType":             "Dynamic",
				"SupportWithoutGpuStart": false,
			},
		},
	}
	confirmCalled := false
	confirmFn := func(action string, args map[string]any) bool {
		confirmCalled = true
		return true
	}
	onStep, _ := collectEvents()

	def := StartInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":        "uhost-yyy",
		"WithoutGpuSpec": "A",
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.False(t, confirmCalled)
	assert.Len(t, executor.calls, 1)
	assert.Contains(t, result.Message, "不支持无卡")
}

func TestStartInstance_PodRejectsWithoutGpuTierB(t *testing.T) {
	executor := startMockExecutor()
	executor.results["DescribeCompShareInstance"] = map[string]any{
		"UHostSet": []any{
			map[string]any{
				"UHostId":                "cpod-yyy",
				"InstanceType":           "Container",
				"State":                  "Stopped",
				"Zone":                   "cn-bj2-04",
				"Region":                 "cn-bj2",
				"GpuType":                "4090",
				"GPU":                    float64(1),
				"ChargeType":             "Dynamic",
				"SupportWithoutGpuStart": true,
			},
		},
	}
	confirmCalled := false
	confirmFn := func(_ string, _ map[string]any) bool {
		confirmCalled = true
		return true
	}
	onStep, _ := collectEvents()

	result, err := NewEngine(executor, confirmFn, onStep).Run(context.Background(), StartInstanceDef(), map[string]any{
		"UHostId":        "cpod-yyy",
		"WithoutGpuSpec": "B",
	})

	require.NoError(t, err)
	assert.False(t, result.Success)
	assert.False(t, confirmCalled)
	assert.Contains(t, result.Message, "容器实例")
	assert.Contains(t, result.Message, "A 档")
	assert.Len(t, executor.calls, 1)
}
