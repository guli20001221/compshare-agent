package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/workflow"
	"github.com/compshare-agent/internal/zones"
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
	avail := map[string]any{"AvailableInstanceTypes": []any{
		map[string]any{"Name": "4090"},
		map[string]any{"Name": "4090_48G"},
		map[string]any{"Name": "TEST_GPU_X"},
	}}

	args, ok := hardwareCreateWorkflowArgs("为我用pytorch最新镜像开一台4090", avail)
	require.True(t, ok)

	assert.Equal(t, "4090", args["GpuType"])
	assert.Equal(t, "torch", args["ImageName"], "PyTorch requests must search the real torch/cuda image names, not fall back to Windows")

	plain, ok := hardwareCreateWorkflowArgs("为我开一台4090", avail)
	require.True(t, ok)
	assert.NotContains(t, plain, "ImageName", "plain hardware creates must not force a framework image")

	bareOpen, ok := hardwareCreateWorkflowArgs("在华北一C用最新 PyTorch 镜像开 4090", avail)
	require.True(t, ok)
	assert.Equal(t, "4090", bareOpen["GpuType"])
	assert.Equal(t, "torch", bareOpen["ImageName"])

	newGPU, ok := hardwareCreateWorkflowArgs("帮我开一台 TEST_GPU_X", avail)
	require.True(t, ok)
	assert.Equal(t, "TEST_GPU_X", newGPU["GpuType"], "new GPU names must come from the upstream availability catalog")
}

func TestDirectHardwareCreateRespectsAgentIntent(t *testing.T) {
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		return map[string]any{"RetCode": float64(0)}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentDeployModel,
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.92,
		},
	}}

	reply, handled := eng.tryDirectHardwareCreate(context.Background(), dispatch, "部署一台4090跑vLLM", noopStep)

	assert.False(t, handled)
	assert.Empty(t, reply)
	assert.Empty(t, executor.calls, "deploy-model intent must not be stolen by the hardware-create fallback")
}

func TestDirectHardwareCreateBlocksConsultation(t *testing.T) {
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		return map[string]any{"RetCode": float64(0)}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentKnowledgeQA,
			Retrieval:     intent.Retrieval{Enabled: true},
			Confidence:    0.9,
		},
	}}

	_, handled := eng.tryDirectHardwareCreate(context.Background(), dispatch, "开4090之前要注意什么", noopStep)

	assert.False(t, handled)
	assert.Empty(t, executor.calls, "consultation turns must stay with the agent/router instead of opening a create card")
}

func TestDirectHardwareCreateBlocksPriceQuestion(t *testing.T) {
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		return map[string]any{"RetCode": float64(0)}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentResourceInfo,
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.9,
		},
	}}

	_, handled := eng.tryDirectHardwareCreate(context.Background(), dispatch, "开一台 4090 多少钱", noopStep)

	assert.False(t, handled)
	assert.Empty(t, executor.calls, "price questions must not open a billable create card")
}

func TestDirectHardwareCreateBlocksPriceQuestionEvenWhenPlannerSaysCreate(t *testing.T) {
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		return map[string]any{"RetCode": float64(0)}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentOperationLifecycle,
			Slots: intent.Slots{
				Action: intent.LifecycleActionCreate,
			},
			Retrieval:  intent.Retrieval{Enabled: false},
			Confidence: 0.9,
		},
	}}

	_, handled := eng.tryDirectHardwareCreate(context.Background(), dispatch, "V100S 实例多少钱", noopStep)

	assert.False(t, handled)
	assert.Empty(t, executor.calls, "executor must re-check price/advice text even if planner mistakenly says create_instance")
}

func TestDirectHardwareCreateObjectCueBlocksNonCreateOperation(t *testing.T) {
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		return map[string]any{"RetCode": float64(0)}, nil
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentOperationLifecycle,
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.9,
		},
	}}

	_, handled := eng.tryDirectHardwareCreate(context.Background(), dispatch, "给一台 V100S 实例加 200G 数据盘", noopStep)

	assert.False(t, handled)
	assert.Empty(t, executor.calls, "object cue fallback must not steal add-disk/resize/reinstall style instance operations")
}

func TestDirectHardwareCreateUsesObjectCueWhenPlannerOmitsAction(t *testing.T) {
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "ZoneId": float64(10027), "Describe": "华北二A", "IsPod": false},
			}}, nil
		case "DescribeCompShareImages":
			return map[string]any{"ImageSet": []any{
				map[string]any{"CompShareImageId": "img-001", "Name": "PyTorch", "ImageType": "App", "Status": "Available"},
			}}, nil
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{
				map[string]any{"Name": "TEST_GPU_X", "Zone": "cn-wlcb-01", "Status": "Normal",
					"MachineSizes": []any{map[string]any{"Gpu": float64(1), "Collection": []any{
						map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}}},
					}}},
			}}, nil
		case "CheckCompShareResourceCapacity":
			return map[string]any{"Specs": []any{
				map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
			}}, nil
		case "GetCompShareInstanceUserPrice":
			return map[string]any{"PriceDetails": []any{
				map[string]any{"ChargeType": "Postpay", "Price": 1.58},
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
	eng.zoneCatalog = zones.NewCatalog(0)
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentOperationLifecycle},
		Model:          "test-planner-model",
	})
	confirm := func(_ string, _ map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		require.NotNil(t, form)
		return workflow.ConfirmResolution{Confirmed: false}
	}

	var workflowEvents []StepEvent
	reply, err := eng.ChatWithOptions(context.Background(), "帮我创一个 TEST_GPU_X 的实例", func(ev StepEvent) {
		workflowEvents = append(workflowEvents, ev)
	}, ChatOptions{GuidedCreate: true, ConfirmEditsFunc: confirm})

	require.NoError(t, err)
	assert.Contains(t, reply, "未执行")
	assert.Empty(t, mock.calls, "semantic create object cue must bypass ReAct tool selection")
	assert.NotContains(t, executor.calls, "CreateCompShareInstance")
	var sawWorkflow bool
	for _, ev := range workflowEvents {
		if ev.Type == StepToolCall && ev.Action == "CreateInstanceWorkflow" {
			sawWorkflow = true
		}
	}
	assert.True(t, sawWorkflow, "operation_lifecycle + real GPU + instance object must open the create workflow even when planner omits slots.action")
}

func TestChat_PlannerCreateInstanceActionDoesNotDependOnHardcodedVerbCue(t *testing.T) {
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "ZoneId": float64(10027), "Describe": "华北二A", "IsPod": false},
			}}, nil
		case "DescribeCompShareImages":
			return map[string]any{"ImageSet": []any{
				map[string]any{"CompShareImageId": "img-001", "Name": "PyTorch", "ImageType": "App", "Status": "Available"},
			}}, nil
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{
				map[string]any{"Name": "V100S", "Zone": "cn-wlcb-01", "Status": "Normal",
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
			Slots: intent.Slots{
				Action: intent.LifecycleActionCreate,
			},
			Retrieval:  intent.Retrieval{Enabled: false},
			Confidence: 0.9,
		},
	}}}
	eng := NewWithDeps(mock, executor, nil)
	eng.zoneCatalog = zones.NewCatalog(0)
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentOperationLifecycle},
		Model:          "test-planner-model",
	})
	var workflowEvents []StepEvent
	confirm := func(_ string, _ map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		require.NotNil(t, form)
		return workflow.ConfirmResolution{Confirmed: false}
	}

	reply, err := eng.ChatWithOptions(context.Background(), "帮我创一个V100S的实例", func(ev StepEvent) {
		workflowEvents = append(workflowEvents, ev)
	}, ChatOptions{GuidedCreate: true, ConfirmEditsFunc: confirm})

	require.NoError(t, err)
	assert.Contains(t, reply, "未执行")
	assert.Empty(t, mock.calls, "planner create_instance action must bypass ReAct tool selection")
	assert.NotContains(t, executor.calls, "CreateCompShareInstance")
	var sawWorkflow bool
	for _, ev := range workflowEvents {
		if ev.Type == StepToolCall && ev.Action == "CreateInstanceWorkflow" {
			sawWorkflow = true
		}
	}
	assert.True(t, sawWorkflow, "planner create_instance action must enter CreateInstanceWorkflow")
}

func TestChat_DirectHardwareCreateDoesNotDependOnPlannerLifecycleIntent(t *testing.T) {
	var availableZone string
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "ZoneId": float64(10027), "Describe": "华北二A", "IsPod": false},
				map[string]any{"Zone": "cn-bj2-03", "Region": "cn-bj2", "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": true},
			}}, nil
		case "DescribeCompShareImages":
			return map[string]any{"ImageSet": []any{
				map[string]any{"CompShareImageId": "img-torch", "Name": "cuda130_torch291_py312", "ImageType": "App", "Status": "Available", "Container": true},
			}}, nil
		case "DescribeAvailableCompShareInstanceTypes":
			availableZone, _ = args["Zone"].(string)
			return map[string]any{"AvailableInstanceTypes": []any{
				map[string]any{"Name": "4090", "Zone": "cn-bj2-03", "Status": "Normal",
					"MachineSizes": []any{
						map[string]any{
							"Gpu": float64(1),
							"Collection": []any{
								map[string]any{"Cpu": float64(16), "Memory": []any{float64(64)}},
							},
						},
					}},
			}}, nil
		case "DescribeCompShareGpuInventory":
			return map[string]any{}, nil
		default:
			return map[string]any{"RetCode": float64(0)}, nil
		}
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "react path must not be reached"}}}
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentResourceInfo,
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.9,
		},
	}}}
	eng := NewWithDeps(mock, executor, nil)
	eng.zoneCatalog = zones.NewCatalog(0)
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentResourceInfo},
		Model:          "test-planner-model",
	})
	confirm := func(_ string, _ map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
		require.NotNil(t, form)
		return workflow.ConfirmResolution{Confirmed: false}
	}

	reply, err := eng.ChatWithOptions(context.Background(), "在华北一C用最新 PyTorch 镜像开 4090", noopStep, ChatOptions{GuidedCreate: true, ConfirmEditsFunc: confirm})

	require.NoError(t, err)
	assert.Contains(t, reply, "未执行")
	assert.Empty(t, mock.calls, "clear hardware-create requests must not depend on the ReAct model choosing the workflow")
	assert.Equal(t, "cn-bj2-03", availableZone)
	assert.Contains(t, executor.calls, "DescribeAvailableCompShareInstanceTypes")
	assert.NotContains(t, executor.calls, "CreateCompShareInstance")
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
	eng.zoneCatalog = zones.NewCatalog(0)
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
