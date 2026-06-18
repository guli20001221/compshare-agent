package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecuteWorkflow_GuidedCreateLocksExplicitGPU(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareImages": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-001", "Name": "PyTorch"},
		}},
		"DescribeAvailableCompShareInstanceTypes": {"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal",
				"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
					map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
				}}}},
			map[string]any{"Name": "4090_48G", "Zone": "cn-wlcb-01", "Status": "Normal",
				"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
					map[string]any{"Cpu": float64(16), "Memory": []any{float64(94)}},
				}}}},
			map[string]any{"Name": "A800", "Zone": "cn-wlcb-01", "Status": "Normal",
				"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
					map[string]any{"Cpu": float64(32), "Memory": []any{float64(128)}},
				}}}},
		}},
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.guidedCreate = true
	var gpuOptions []string
	eng.confirmEditsFn = func(_ string, _ map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		require.NotNil(t, form)
		require.NotNil(t, form.Step)
		if form.Step.Index == 1 {
			gpu := form.Field("GpuType")
			require.NotNil(t, gpu)
			for _, opt := range gpu.Options {
				gpuOptions = append(gpuOptions, opt.Value)
			}
		}
		return workflow.ConfirmResolution{Confirmed: false}
	}

	_ = eng.executeWorkflow(context.Background(), "CreateInstanceWorkflow", map[string]any{"GpuType": "4090"}, noopStep)

	assert.Equal(t, []string{"4090", "4090_48G"}, gpuOptions)
}

func TestExecuteWorkflow_GuidedCreateCanonicalizesSpaced409048G(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareImages": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-001", "Name": "PyTorch"},
		}},
		"DescribeAvailableCompShareInstanceTypes": {"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal",
				"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
					map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
				}}}},
			map[string]any{"Name": "4090_48G", "Zone": "cn-wlcb-01", "Status": "Normal",
				"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
					map[string]any{"Cpu": float64(16), "Memory": []any{float64(94)}},
				}}}},
		}},
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.guidedCreate = true
	var gpuOptions []string
	var seenGpuArgs []string
	eng.confirmEditsFn = func(_ string, args map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		require.NotNil(t, form)
		require.NotNil(t, form.Step)
		if gt, _ := args["GpuType"].(string); gt != "" {
			seenGpuArgs = append(seenGpuArgs, gt)
		}
		if gpu := form.Field("GpuType"); gpu != nil {
			for _, opt := range gpu.Options {
				gpuOptions = append(gpuOptions, opt.Value)
			}
		}
		return workflow.ConfirmResolution{Confirmed: false}
	}

	_ = eng.executeWorkflow(context.Background(), "CreateInstanceWorkflow", map[string]any{"GpuType": "4090 48G"}, noopStep)

	assert.NotEmpty(t, seenGpuArgs)
	assert.Equal(t, "4090_48G", seenGpuArgs[0])
	assert.NotContains(t, seenGpuArgs, "4090 48G")
	if len(gpuOptions) > 0 {
		assert.Equal(t, []string{"4090_48G"}, gpuOptions)
	}
}

func TestExecuteWorkflow_GuidedCreateUsesExplicit409048GFromUserText(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareImages": {"ImageSet": []any{
			map[string]any{"CompShareImageId": "img-001", "Name": "PyTorch"},
		}},
		"DescribeAvailableCompShareInstanceTypes": {"AvailableInstanceTypes": []any{
			map[string]any{"Name": "4090", "Zone": "cn-wlcb-01", "Status": "Normal",
				"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
					map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
				}}}},
			map[string]any{"Name": "4090_48G", "Zone": "cn-wlcb-01", "Status": "Normal",
				"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
					map[string]any{"Cpu": float64(16), "Memory": []any{float64(94)}},
				}}}},
		}},
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.guidedCreate = true
	eng.lastUserMsg = "开一台 4090 48G"
	var seenGpuArgs []string
	eng.confirmEditsFn = func(_ string, args map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		require.NotNil(t, form)
		if gt, _ := args["GpuType"].(string); gt != "" {
			seenGpuArgs = append(seenGpuArgs, gt)
		}
		return workflow.ConfirmResolution{Confirmed: false}
	}

	_ = eng.executeWorkflow(context.Background(), "CreateInstanceWorkflow", map[string]any{"GpuType": "4090"}, noopStep)

	assert.NotEmpty(t, seenGpuArgs)
	assert.Equal(t, "4090_48G", seenGpuArgs[0])
	assert.NotContains(t, seenGpuArgs, "4090")
}

func TestHardwareCreateWorkflowArgsCarriesImageIntent(t *testing.T) {
	args, ok := hardwareCreateWorkflowArgs("为我用pytorch最新镜像开一台4090")
	require.True(t, ok)

	assert.Equal(t, "4090", args["GpuType"])
	assert.Equal(t, "torch", args["ImageName"], "PyTorch requests must search the real torch/cuda image names, not fall back to Windows")

	plain, ok := hardwareCreateWorkflowArgs("为我开一台4090")
	require.True(t, ok)
	assert.NotContains(t, plain, "ImageName", "plain hardware creates must not force a framework image")
}

func TestChat_DirectHardwareCreateForGrabShanghai4090(t *testing.T) {
	var availableZone string
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "ZoneId": float64(10027), "Describe": "华北二A", "IsPod": false},
				map[string]any{"Zone": "cn-sh2-02", "Region": "cn-sh2", "ZoneId": float64(8200), "Describe": "上海二B", "IsPod": false},
			}}, nil
		case "DescribeCompShareImages":
			return map[string]any{"ImageSet": []any{
				map[string]any{"CompShareImageId": "img-001", "Name": "PyTorch"},
			}}, nil
		case "DescribeAvailableCompShareInstanceTypes":
			availableZone, _ = args["Zone"].(string)
			return map[string]any{"AvailableInstanceTypes": []any{
				map[string]any{"Name": "4090", "Zone": "cn-sh2-02", "Status": "Normal",
					"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
						map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
					}}}},
			}}, nil
		default:
			return map[string]any{"RetCode": float64(0)}, nil
		}
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "react path must not be reached"}}}
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentOperationLifecycle,
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.9,
		},
	}}}
	eng := NewWithDeps(mock, executor, nil)
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentOperationLifecycle},
		Model:          "test-planner-model",
	})
	var workflowEvents []StepEvent
	confirm := func(_ string, _ map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		require.NotNil(t, form)
		return workflow.ConfirmResolution{Confirmed: false}
	}

	reply, err := eng.ChatWithOptions(context.Background(), "帮我抢一台上海的 4090", func(ev StepEvent) {
		workflowEvents = append(workflowEvents, ev)
	}, ChatOptions{GuidedCreate: true, ConfirmEditsFunc: confirm})

	require.NoError(t, err)
	assert.Contains(t, reply, "未执行")
	assert.Empty(t, mock.calls, "clear hardware-create requests must not depend on the ReAct model choosing the workflow")
	assert.Equal(t, "cn-sh2-02", availableZone)
	assert.Contains(t, executor.calls, "DescribeAvailableCompShareInstanceTypes")
	assert.NotContains(t, executor.calls, "CreateCompShareInstance")
	var sawWorkflow bool
	for _, ev := range workflowEvents {
		if ev.Type == StepToolCall && ev.Action == "CreateInstanceWorkflow" {
			sawWorkflow = true
		}
	}
	assert.True(t, sawWorkflow, "direct hardware create must emit the CreateInstanceWorkflow step")
}
