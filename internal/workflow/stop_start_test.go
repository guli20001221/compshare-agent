package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
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
	assert.Contains(t, warning, "系统盘 100GB 免费")
	assert.Contains(t, warning, "挂载数据盘")
	assert.Contains(t, warning, "系统盘扩容超出 100GB")
	assert.NotContains(t, warning, "磁盘费用仍会产生，如需彻底停止计费")
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
	assert.Equal(t, "开机", result.StoppedAt)
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

func TestStopInstance_SpotInstanceSendsForce(t *testing.T) {
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
	assert.Equal(t, true, stopCall.args["Force"], "Spot instance stop must include Force=true")
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

func TestStartInstance_WithoutGpuResizesThenStarts(t *testing.T) {
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
	executor.results["ResizeCompShareInstance"] = map[string]any{"RetCode": 0}
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := StartInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":    "uhost-yyy",
		"WithoutGpu": true,
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Len(t, executor.calls, 3)
	queryCall := executor.calls[0]
	assert.Equal(t, "DescribeCompShareInstance", queryCall.action)
	assert.NotContains(t, queryCall.args, "WithoutGpu", "DescribeCompShareInstance must query the source instance normally")
	resizeCall := executor.calls[1]
	assert.Equal(t, "ResizeCompShareInstance", resizeCall.action)
	assert.Equal(t, true, resizeCall.args["WithoutGpu"])
	assert.Equal(t, float64(2), resizeCall.args["Cpu"])
	assert.Equal(t, float64(4096), resizeCall.args["Memory"])
	assert.Equal(t, float64(0), resizeCall.args["Gpu"])
	startCall := executor.calls[2]
	assert.Equal(t, "StartCompShareInstance", startCall.action)
	assert.NotContains(t, startCall.args, "WithoutGpu", "StartCompShareInstance does not accept WithoutGpu")
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
		"UHostId":    "uhost-yyy",
		"WithoutGpu": true,
	})

	assert.Contains(t, capturedArgs, "mode", "Confirm summary must show mode when WithoutGpu=true")
	assert.Equal(t, float64(2), capturedArgs["without_gpu_cpu"])
	assert.Equal(t, float64(4096), capturedArgs["without_gpu_memory"])
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
			},
		},
	}
	executor.results["ResizeCompShareInstance"] = map[string]any{"RetCode": 0}
	confirmFn := func(action string, args map[string]any) bool { return true }
	onStep, _ := collectEvents()

	def := StartInstanceDef()
	eng := NewEngine(executor, confirmFn, onStep)
	result, err := eng.Run(context.Background(), def, map[string]any{
		"UHostId":    "uhost-yyy",
		"WithoutGpu": true,
	})

	assert.NoError(t, err)
	assert.True(t, result.Success)
	assert.Len(t, executor.calls, 3)
	resizeCall := executor.calls[1]
	assert.Equal(t, "ResizeCompShareInstance", resizeCall.action)
	assert.Equal(t, true, resizeCall.args["WithoutGpu"])
	assert.Equal(t, float64(2), resizeCall.args["Cpu"])
	assert.Equal(t, float64(4096), resizeCall.args["Memory"])
	assert.Equal(t, float64(0), resizeCall.args["Gpu"])
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
		"UHostId":    "uhost-yyy",
		"WithoutGpu": true,
	})

	assert.NoError(t, err)
	assert.False(t, result.Success)
	assert.False(t, confirmCalled)
	assert.Len(t, executor.calls, 1)
	assert.Contains(t, result.Message, "不支持无卡")
}
