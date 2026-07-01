package engine

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/zones"
)

func createDispatch() routerDispatchResult {
	return routerDispatchResult{
		result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentCreateInstance}},
	}
}

type fakeContextContinuationResolver struct {
	decision *ContextContinuationDecision
	err      error
	calls    []ContextContinuationInput
}

func (f *fakeContextContinuationResolver) ResolveContextContinuation(_ context.Context, in ContextContinuationInput) (*ContextContinuationDecision, error) {
	f.calls = append(f.calls, in)
	if f.err != nil {
		return nil, f.err
	}
	return f.decision, nil
}

func TestUnifiedCreateFlag_DefaultOn(t *testing.T) {
	SetUnifiedCreateEnabled(true)
	assert.True(t, UnifiedCreateEnabled())
}

func TestDispatchAgentSkill_CreateInstanceFlagOffFallsThrough(t *testing.T) {
	SetUnifiedCreateEnabled(false)
	t.Cleanup(func() { SetUnifiedCreateEnabled(true) })
	exec := newDeployMock(deployMockConfig{capacityEnough: true})
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { return true })

	reply, handled := eng.dispatchAgentSkill(context.Background(), createDispatch(), "创建一台4090", noopStep)

	assert.False(t, handled)
	assert.Empty(t, reply)
	assert.Empty(t, exec.calls)
}

func TestTryPlannerDispatch_UnknownClearsPendingDeployModel(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{
		SchemaVersion:      SessionStateSchemaV1,
		LastIntent:         string(intent.IntentDeployModel),
		PendingDeployModel: "DeepSeek R1",
	}, 1)
	eng.SetIntentPlanner(&scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentUnknown,
			Slots:         intent.Slots{TargetRefs: []intent.TargetRef{}, Metrics: []intent.Metric{}},
			RequiredTools: []string{},
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.9,
		},
	}}}, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentResourceInfo}})

	_, _ = eng.tryPlannerDispatch(context.Background(), "随便问个无关问题", "", noopStep, nil)

	state, _, _ := eng.SessionStateSnapshot()
	assert.Empty(t, state.PendingDeployModel)
}

func TestDispatchAgentSkill_CreateInstanceFlagOnSpecOnlyStartsCreateWorkflowWithoutImageMatch(t *testing.T) {
	SetUnifiedCreateEnabled(true)
	t.Cleanup(func() { SetUnifiedCreateEnabled(true) })
	SetCreatePreferenceExtractionEnabled(true)
	t.Cleanup(func() { SetCreatePreferenceExtractionEnabled(true) })

	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	client := &mockLLM{responses: []llm.ChatResponse{{Content: deploySearchJSON}, {Content: deployMatchJSON}}}
	eng := NewWithDeps(client, exec, func(string, map[string]any) bool { return false })
	eng.guidedCreate = true
	eng.SetCreatePreferenceExtractor(&fakeCreatePreferenceExtractor{result: &CreatePreferenceExtractionResult{}})
	onStep, events := collectSteps()

	reply, handled := eng.dispatchAgentSkill(context.Background(), createDispatch(), "创建一台4090", onStep)

	require.True(t, handled)
	assert.Contains(t, reply, "创建实例")
	assert.Empty(t, client.calls, "pure hardware create must not call the deploy image matcher LLM")
	assert.Equal(t, 0, countCalls(exec.calls, "CreateCompShareInstance"), "confirm=false path must not create")
	require.NotEmpty(t, *events)
	assert.True(t, sawStepAction(*events, "CreateInstanceWorkflow"))
}

func TestDispatchAgentSkill_CreateInstancePreferenceFlagOffDoesNotCallExtractor(t *testing.T) {
	SetUnifiedCreateEnabled(true)
	t.Cleanup(func() { SetUnifiedCreateEnabled(true) })
	SetCreatePreferenceExtractionEnabled(false)
	t.Cleanup(func() { SetCreatePreferenceExtractionEnabled(true) })

	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	client := &mockLLM{responses: []llm.ChatResponse{{Content: deploySearchJSON}, {Content: deployMatchJSON}}}
	eng := NewWithDeps(client, exec, func(string, map[string]any) bool { return false })
	eng.guidedCreate = true
	extractor := &fakeCreatePreferenceExtractor{result: &CreatePreferenceExtractionResult{ImagePref: "PyTorch"}}
	eng.SetCreatePreferenceExtractor(extractor)
	onStep, events := collectSteps()

	reply, handled := eng.dispatchAgentSkill(context.Background(), createDispatch(), "创建一台4090装PyTorch", onStep)

	require.True(t, handled)
	assert.Contains(t, reply, "创建实例")
	assert.Empty(t, extractor.calls, "flag off must disable create_instance preference extraction")
	assert.Empty(t, client.calls, "flag off must not route image preference through deploy matcher")
	assert.Equal(t, 0, countCalls(exec.calls, "CreateCompShareInstance"), "confirm=false path must not create")
	require.NotEmpty(t, *events)
	args := workflowStepArgs(t, *events, "CreateInstanceWorkflow")
	assert.Equal(t, "4090", args["GpuType"])
}

func TestDispatchAgentSkill_CreateInstanceV100VariantsStartCreateWorkflow(t *testing.T) {
	cases := []string{
		"为我创一台V100S的实例",
		"帮我创建一台V100S的实例",
		"开一台V100S实例",
		"为我创建一台v100实例",
	}
	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			SetUnifiedCreateEnabled(true)
			t.Cleanup(func() { SetUnifiedCreateEnabled(true) })

			exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
			client := &mockLLM{}
			eng := NewWithDeps(client, exec, func(string, map[string]any) bool { return false })
			eng.guidedCreate = true
			eng.SetCreatePreferenceExtractor(&fakeCreatePreferenceExtractor{result: &CreatePreferenceExtractionResult{}})
			onStep, events := collectSteps()

			reply, handled := eng.dispatchAgentSkill(context.Background(), createDispatch(), msg, onStep)

			require.True(t, handled)
			assert.Contains(t, reply, "创建实例")
			assert.Empty(t, client.calls, "pure hardware create must not call the deploy image matcher LLM")
			assert.Equal(t, 0, countCalls(exec.calls, "CreateCompShareInstance"), "confirm=false path must not create")
			require.NotEmpty(t, *events)
			args := workflowStepArgs(t, *events, "CreateInstanceWorkflow")
			assert.Equal(t, "V100S", args["GpuType"])
		})
	}
}

func TestOperationLifecycleCreateDiskUsesDiskWorkflow(t *testing.T) {
	var priceArgs map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-disk-1",
				"Name":    "gate1-v100s",
				"State":   "Running",
				"GpuType": "V100S",
				"Gpu":     float64(1),
				"Cpu":     float64(10),
				"Memory":  float64(65536),
				"Region":  "cn-wlcb",
				"Zone":    "cn-wlcb-01",
			}}}, nil
		case "GetCompShareInstancePrice":
			priceArgs = args
			return map[string]any{"PriceDetails": []any{map[string]any{
				"Disks": float64(0.02),
			}}}, nil
		default:
			return map[string]any{"RetCode": float64(0)}, nil
		}
	}}
	eng := NewWithDeps(&mockLLM{}, exec, func(string, map[string]any) bool { return false })
	onStep, events := collectSteps()
	dispatch := routerDispatchResult{
		result: intent.IntentRouterResult{Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentOperationLifecycle,
			Slots: intent.Slots{
				Action: intent.LifecycleActionCreateDisk,
				TargetRefs: []intent.TargetRef{{
					Type:   intent.TargetRefName,
					Value:  "gate1-v100s",
					Source: intent.SourceUserText,
				}},
			},
		}},
	}
	dispatch.result.Plan.Slots.TargetRefs = []intent.TargetRef{{Type: intent.TargetRefName, Value: "gate1-v100s", Source: intent.SourceUserText}}
	dispatch.snapshot = entity.RegistrySnapshot{
		Instances: map[string]entity.InstanceSnapshot{
			"uhost-disk-1": {
				UHostId: "uhost-disk-1",
				Name:    "gate1-v100s",
				State:   "Running",
				GpuType: "V100S",
				GPU:     1,
				CPU:     10,
				Memory:  65536,
				Region:  "cn-wlcb",
				Zone:    "cn-wlcb-01",
			},
		},
		NameIndex:    map[string][]string{"gate1v100s": {"uhost-disk-1"}},
		LastFullSync: time.Now(),
	}

	reply, handled := eng.tryOperationLifecycleDispatch(context.Background(), dispatch, "给 gate1-v100s 创建一块 20G 数据盘", onStep)

	require.True(t, handled)
	assert.Contains(t, reply, "操作未执行")
	assert.False(t, sawStepAction(*events, "CreateInstanceWorkflow"))
	assert.Equal(t, 1, countCalls(exec.calls, "GetCompShareInstancePrice"))
	require.NotNil(t, priceArgs)
	assert.Equal(t, "V100S", priceArgs["GpuType"])
	assert.Equal(t, float64(1), priceArgs["Gpu"])
	assert.Equal(t, float64(10), priceArgs["Cpu"])
	assert.Equal(t, float64(65536), priceArgs["Memory"])
	disks, ok := priceArgs["Disks"].([]any)
	require.True(t, ok)
	require.Len(t, disks, 1)
	disk, ok := disks[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, float64(20), disk["Size"])
}

func TestDispatchAgentSkill_CreateInstanceSpecOnlyResolvesZonePreference(t *testing.T) {
	SetUnifiedCreateEnabled(true)
	t.Cleanup(func() { SetUnifiedCreateEnabled(true) })
	SetCreatePreferenceExtractionEnabled(true)
	t.Cleanup(func() { SetCreatePreferenceExtractionEnabled(true) })

	exec := newDeployMockWithSupportZones(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	client := &mockLLM{}
	eng := NewWithDeps(client, exec, func(string, map[string]any) bool { return false })
	eng.zoneCatalog = zones.NewCatalog(0)
	eng.SetCreatePreferenceExtractor(&fakeCreatePreferenceExtractor{result: &CreatePreferenceExtractionResult{
		GPUPref:  "4090",
		ZonePref: "华北二A",
	}})
	onStep, events := collectSteps()

	reply, handled := eng.dispatchAgentSkill(context.Background(), createDispatch(), "在华北二A创建一台4090", onStep)

	require.True(t, handled)
	assert.Contains(t, reply, "创建实例")
	require.NotEmpty(t, *events)
	args := workflowStepArgs(t, *events, "CreateInstanceWorkflow")
	assert.Equal(t, "4090", args["GpuType"])
	assert.Equal(t, "cn-wlcb-01", args["Zone"])
	assert.Empty(t, client.calls, "pure hardware create must not call the deploy image matcher LLM")
}

func TestDispatchAgentSkill_CreateInstanceImagePreferenceUsesDeployMatcher(t *testing.T) {
	SetUnifiedCreateEnabled(true)
	t.Cleanup(func() { SetUnifiedCreateEnabled(true) })
	SetCreatePreferenceExtractionEnabled(true)
	t.Cleanup(func() { SetCreatePreferenceExtractionEnabled(true) })

	base := newDeployMockWithSupportZones(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	var createArgs map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "CreateCompShareInstance" {
			createArgs = args
		}
		return base.fn(action, args)
	}}
	client := &mockLLM{responses: []llm.ChatResponse{{Content: deploySearchJSON}, {Content: deployMatchJSON}}}
	eng := NewWithDeps(client, exec, okConfirm)
	eng.zoneCatalog = zones.NewCatalog(0)
	eng.SetCreatePreferenceExtractor(&fakeCreatePreferenceExtractor{result: &CreatePreferenceExtractionResult{
		GPUPref:     "4090",
		ImagePref:   "PyTorch",
		ImageSource: "platform",
	}})

	reply, handled := eng.dispatchAgentSkill(context.Background(), createDispatch(), "创建一台4090装PyTorch", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "uhost-deploy-1")
	require.NotEmpty(t, client.calls, "image preference create must reuse the deploy image matcher")
	require.NotNil(t, createArgs)
	assert.Equal(t, "4090", createArgs["GpuType"])
	assert.Equal(t, "img-pt", createArgs["CompShareImageId"])
	_, hasImageName := createArgs["ImageName"]
	assert.False(t, hasImageName, "fuzzy image preference must not be passed as the final create image name")
}

func TestDispatchAgentSkill_CreateInstanceCommunityImagePreferenceUsesDeployMatcher(t *testing.T) {
	SetUnifiedCreateEnabled(true)
	t.Cleanup(func() { SetUnifiedCreateEnabled(true) })
	SetCreatePreferenceExtractionEnabled(true)
	t.Cleanup(func() { SetCreatePreferenceExtractionEnabled(true) })

	base := newDeployMockWithSupportZones(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}, communityImageID: "comm-img-9"})
	var createArgs map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "CreateCompShareInstance" {
			createArgs = args
		}
		return base.fn(action, args)
	}}
	client := &mockLLM{responses: []llm.ChatResponse{
		{Content: deploySearchJSON},
		{Content: `{"image_source":"community","image_name":"LiveTalking","model_name":"","quantization":""}`},
	}}
	eng := NewWithDeps(client, exec, okConfirm)
	eng.zoneCatalog = zones.NewCatalog(0)
	eng.SetCreatePreferenceExtractor(&fakeCreatePreferenceExtractor{result: &CreatePreferenceExtractionResult{
		GPUPref:     "4090",
		ImageSource: "community",
	}})

	reply, handled := eng.dispatchAgentSkill(context.Background(), createDispatch(), "用社区镜像创建一台4090", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "uhost-deploy-1")
	require.NotEmpty(t, client.calls, "community image preference must reuse the deploy image matcher")
	require.NotNil(t, createArgs)
	assert.Equal(t, "4090", createArgs["GpuType"])
	assert.Equal(t, "comm-img-9", createArgs["CompShareImageId"])
}

func TestDispatchAgentSkill_CreateInstanceEmptyInputDoesNotOpenCreateCard(t *testing.T) {
	SetUnifiedCreateEnabled(true)
	t.Cleanup(func() { SetUnifiedCreateEnabled(true) })

	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { return true })

	reply, handled := eng.dispatchAgentSkill(context.Background(), createDispatch(), "   ", noopStep)

	assert.True(t, handled)
	assert.Contains(t, reply, "不会直接创建")
	assert.Equal(t, 0, countCalls(exec.calls, "CreateCompShareInstance"))
}

func TestDispatchAgentSkill_UnifiedCreateMixedWorkloadUsesDeployMatcherWithPinnedGPU(t *testing.T) {
	SetUnifiedCreateEnabled(true)
	SetCreatePreferenceExtractionEnabled(true)
	t.Cleanup(func() {
		SetUnifiedCreateEnabled(true)
		SetCreatePreferenceExtractionEnabled(true)
	})

	base := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	var createArgs map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "CreateCompShareInstance" {
			createArgs = args
		}
		return base.fn(action, args)
	}}
	client := &mockLLM{responses: []llm.ChatResponse{
		{Content: `{"workload_pref":"Qwen","gpu_pref":"4090"}`},
		{Content: deploySearchJSON},
		{Content: deployMatchJSON},
	}}
	eng := NewWithDeps(client, exec, func(string, map[string]any) bool { return true })

	reply, handled := eng.dispatchAgentSkill(context.Background(), createDispatch(), "部署一台4090跑Qwen", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "uhost-deploy-1")
	require.NotEmpty(t, client.calls, "mixed create must reuse the deploy image matcher")
	require.NotNil(t, createArgs)
	assert.Equal(t, "4090", createArgs["GpuType"], "user-named GPU must stay pinned in the final create params")
}

func TestResumeCreateContextFrame_ZoneFollowupContinuesPriorDeploy(t *testing.T) {
	SetContextContinuationEnabled(true)
	SetUnifiedCreateEnabled(true)
	SetCreatePreferenceExtractionEnabled(true)
	t.Cleanup(func() {
		SetContextContinuationEnabled(false)
		SetUnifiedCreateEnabled(true)
		SetCreatePreferenceExtractionEnabled(true)
	})

	var createArgs map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCommunityImages":
			return map[string]any{"CompshareImageGroup": []any{}}, nil
		case "DescribeCompShareImages":
			return map[string]any{"ImageSet": []any{map[string]any{
				"CompShareImageId":  "img-pt",
				"Name":              "PyTorch",
				"ImageType":         "App",
				"Status":            "Available",
				"SupportedGpuTypes": []any{"4090"},
			}}}, nil
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{availCardZ("4090", "cn-wlcb-01", 24)}}, nil
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-bj2-03", "Region": "cn-bj2", "RegionId": float64(3003), "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": false},
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(10027), "Describe": "华北二A", "IsPod": false},
			}}, nil
		case "CheckCompShareResourceCapacity":
			return map[string]any{"Specs": []any{
				map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
			}}, nil
		case "GetCompShareInstanceUserPrice":
			return map[string]any{"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Price": 1.23}}}, nil
		case "CreateCompShareInstance":
			createArgs = args
			return map[string]any{"UHostIds": []any{"uhost-resume-1"}}, nil
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{"UHostId": "uhost-resume-1", "State": "Running", "GpuType": "4090"}}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	client := &mockLLM{responses: []llm.ChatResponse{
		{Content: deploySearchJSON},
		{Content: `{"image_source":"platform","image_name":"PyTorch","model_name":"Qwen2.5-7B","match_kind":"base","quantization":""}`},
	}}
	eng := NewWithDeps(client, exec, okConfirm)
	eng.zoneCatalog = zones.NewCatalog(0)
	eng.SetContextContinuationResolver(&fakeContextContinuationResolver{decision: &ContextContinuationDecision{
		Decision: ContextContinuationContinue,
		ZonePref: "华北二A",
	}})
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Version:         1,
			Kind:            ContextFrameKindDeploy,
			Status:          ContextFrameStatusFailedRecoverable,
			Intent:          string(intent.IntentDeployModel),
			OriginalUserMsg: "在华北一C用最新pytorch给我开一台",
			GPU:             "4090",
			ImagePref:       "PyTorch",
			ImageSource:     "platform",
			Zone:            "cn-bj2-03",
			ZoneLabel:       "华北一C",
			FailureReason:   "华北一C 暂无库存",
			ProducedAtUnix:  time.Now().Unix(),
			TTLSeconds:      ContextFrameTTLSeconds,
		},
	}, 1)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentStockAvailability}}}

	reply, handled := eng.tryResumeCreateContextFrame(context.Background(), dispatch, "那华北二A呢", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "uhost-resume-1")
	require.NotNil(t, createArgs)
	assert.Equal(t, "cn-wlcb-01", createArgs["Zone"], "follow-up must use the real zone id, not the Chinese display name")
	assert.Equal(t, "4090", createArgs["GpuType"])
	state, _, _ := eng.SessionStateSnapshot()
	assert.Empty(t, state.ContextFrame.Kind, "successful continuation clears the pending create frame")
}

func TestResumeCreateContextFrame_ContextContinuationFlagOffDoesNotCallResolver(t *testing.T) {
	SetContextContinuationEnabled(false)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	resolver := &fakeContextContinuationResolver{decision: &ContextContinuationDecision{
		Decision: ContextContinuationContinue,
		ZonePref: "华北二A",
	}}
	eng := NewWithDeps(&mockLLM{}, newDeployMockWithSupportZones(deployMockConfig{capacityEnough: true}), okConfirm)
	eng.SetContextContinuationResolver(resolver)
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindDeploy,
			Status:         ContextFrameStatusFailedRecoverable,
			Intent:         string(intent.IntentDeployModel),
			GPU:            "4090",
			ImagePref:      "PyTorch",
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     ContextFrameTTLSeconds,
		},
	}, 1)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentStockAvailability}}}

	reply, handled := eng.tryResumeCreateContextFrame(context.Background(), dispatch, "那华北二A呢", noopStep)

	assert.False(t, handled)
	assert.Empty(t, reply)
	assert.Empty(t, resolver.calls, "flag-off must not call the context resolver")
}

func TestResumeCreateContextFrame_NoFrameDoesNotInventCreate(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, newDeployMockWithSupportZones(deployMockConfig{capacityEnough: true}), okConfirm)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentStockAvailability}}}

	reply, handled := eng.tryResumeCreateContextFrame(context.Background(), dispatch, "那华北二A呢", noopStep)

	assert.False(t, handled)
	assert.Empty(t, reply)
}

func TestResumeCreateContextFrame_PricingPlanDoesNotResumeCreate(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	eng := NewWithDeps(&mockLLM{}, newDeployMockWithSupportZones(deployMockConfig{capacityEnough: true}), okConfirm)
	eng.SetContextContinuationResolver(&fakeContextContinuationResolver{decision: &ContextContinuationDecision{
		Decision: ContextContinuationNew,
	}})
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindDeploy,
			Status:         ContextFrameStatusFailedRecoverable,
			Intent:         string(intent.IntentDeployModel),
			GPU:            "4090",
			ImagePref:      "PyTorch",
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     ContextFrameTTLSeconds,
		},
	}, 1)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentPricingQuery}}}

	reply, handled := eng.tryResumeCreateContextFrame(context.Background(), dispatch, "华北二A多少钱", noopStep)

	assert.False(t, handled)
	assert.Empty(t, reply)
	state, _, _ := eng.SessionStateSnapshot()
	assert.Empty(t, state.ContextFrame.Kind, "new standalone questions clear the stale create frame")
}

func TestResumeCreateContextFrame_GpuFollowupContinuesPriorDeploy(t *testing.T) {
	SetContextContinuationEnabled(true)
	SetUnifiedCreateEnabled(true)
	SetCreatePreferenceExtractionEnabled(true)
	t.Cleanup(func() {
		SetContextContinuationEnabled(false)
		SetUnifiedCreateEnabled(true)
		SetCreatePreferenceExtractionEnabled(true)
	})

	var createArgs map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCommunityImages":
			return map[string]any{"CompshareImageGroup": []any{}}, nil
		case "DescribeCompShareImages":
			return map[string]any{"ImageSet": []any{map[string]any{
				"CompShareImageId":  "img-pt",
				"Name":              "PyTorch",
				"ImageType":         "App",
				"Status":            "Available",
				"SupportedGpuTypes": []any{"5090"},
			}}}, nil
		case "DescribeAvailableCompShareInstanceTypes":
			return map[string]any{"AvailableInstanceTypes": []any{
				availCardZ("4090", "cn-bj2-03", 24),
				availCardZ("5090", "cn-bj2-03", 32),
			}}, nil
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-bj2-03", "Region": "cn-bj2", "RegionId": float64(3003), "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": false},
			}}, nil
		case "CheckCompShareResourceCapacity":
			return map[string]any{"Specs": []any{
				map[string]any{"Gpu": float64(1), "Cpu": float64(16), "Mem": float64(64), "ResourceEnough": true},
			}}, nil
		case "GetCompShareInstanceUserPrice":
			return map[string]any{"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Price": 1.23}}}, nil
		case "CreateCompShareInstance":
			createArgs = args
			return map[string]any{"UHostIds": []any{"uhost-resume-gpu"}}, nil
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{"UHostId": "uhost-resume-gpu", "State": "Running", "GpuType": "5090"}}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	client := &mockLLM{responses: []llm.ChatResponse{
		{Content: deploySearchJSON},
		{Content: `{"image_source":"platform","image_name":"PyTorch","model_name":"Qwen2.5-7B","match_kind":"base","quantization":""}`},
	}}
	eng := NewWithDeps(client, exec, okConfirm)
	eng.zoneCatalog = zones.NewCatalog(0)
	eng.SetContextContinuationResolver(&fakeContextContinuationResolver{decision: &ContextContinuationDecision{
		Decision: ContextContinuationContinue,
		GPUPref:  "5090",
	}})
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Version:         1,
			Kind:            ContextFrameKindDeploy,
			Status:          ContextFrameStatusFailedRecoverable,
			Intent:          string(intent.IntentDeployModel),
			OriginalUserMsg: "在华北一C用最新pytorch给我开一台4090",
			GPU:             "4090",
			ImagePref:       "PyTorch",
			ImageSource:     "platform",
			Zone:            "cn-bj2-03",
			ZoneLabel:       "华北一C",
			FailureReason:   "4090 暂无库存",
			ProducedAtUnix:  time.Now().Unix(),
			TTLSeconds:      ContextFrameTTLSeconds,
		},
	}, 1)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentStockAvailability}}}

	reply, handled := eng.tryResumeCreateContextFrame(context.Background(), dispatch, "那5090呢", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "uhost-resume-gpu")
	require.NotNil(t, createArgs)
	assert.Equal(t, "5090", createArgs["GpuType"], "GPU follow-up must use the live catalog match, not the stale frame GPU")
	assert.Equal(t, "cn-bj2-03", createArgs["Zone"], "GPU follow-up should keep the previous explicit zone when no new zone is provided")
}

func TestResumeCreateContextFrame_ImageSourceFollowupContinuesPriorDeploy(t *testing.T) {
	SetContextContinuationEnabled(true)
	SetUnifiedCreateEnabled(true)
	SetCreatePreferenceExtractionEnabled(true)
	t.Cleanup(func() {
		SetContextContinuationEnabled(false)
		SetUnifiedCreateEnabled(true)
		SetCreatePreferenceExtractionEnabled(true)
	})

	base := newDeployMockWithSupportZones(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}, communityImageID: "comm-img-9"})
	var createArgs map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "CreateCompShareInstance" {
			createArgs = args
		}
		return base.fn(action, args)
	}}
	client := &mockLLM{responses: []llm.ChatResponse{
		{Content: deploySearchJSON},
		{Content: `{"image_source":"community","image_name":"LiveTalking","model_name":"","match_kind":"base","quantization":""}`},
	}}
	eng := NewWithDeps(client, exec, okConfirm)
	eng.zoneCatalog = zones.NewCatalog(0)
	eng.SetContextContinuationResolver(&fakeContextContinuationResolver{decision: &ContextContinuationDecision{
		Decision:    ContextContinuationContinue,
		ImageSource: "community",
	}})
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindDeploy,
			Status:         ContextFrameStatusFailedRecoverable,
			Intent:         string(intent.IntentDeployModel),
			GPU:            "4090",
			ImagePref:      "PyTorch",
			ImageSource:    "platform",
			Zone:           "cn-wlcb-01",
			ZoneLabel:      "华北二A",
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     ContextFrameTTLSeconds,
		},
	}, 1)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentKnowledgeQA}}}

	reply, handled := eng.tryResumeCreateContextFrame(context.Background(), dispatch, "换成社区镜像呢", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "uhost-deploy-1")
	require.NotNil(t, createArgs)
	assert.Equal(t, "comm-img-9", createArgs["CompShareImageId"])
}

func TestResumeCreateContextFrame_ClearDecisionDropsFrame(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	eng := NewWithDeps(&mockLLM{}, newDeployMockWithSupportZones(deployMockConfig{capacityEnough: true}), okConfirm)
	eng.SetContextContinuationResolver(&fakeContextContinuationResolver{decision: &ContextContinuationDecision{
		Decision: ContextContinuationClear,
	}})
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame: ContextFrame{
			Version:        1,
			Kind:           ContextFrameKindDeploy,
			Status:         ContextFrameStatusFailedRecoverable,
			GPU:            "4090",
			ProducedAtUnix: time.Now().Unix(),
			TTLSeconds:     ContextFrameTTLSeconds,
		},
	}, 1)
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentPricingQuery}}}

	reply, handled := eng.tryResumeCreateContextFrame(context.Background(), dispatch, "算了，4090多少钱", noopStep)

	assert.False(t, handled)
	assert.Empty(t, reply)
	state, _, _ := eng.SessionStateSnapshot()
	assert.Empty(t, state.ContextFrame.Kind)
}

func sawStepAction(events []StepEvent, action string) bool {
	for _, ev := range events {
		if ev.Action == action {
			return true
		}
	}
	return false
}

func workflowStepArgs(t *testing.T, events []StepEvent, action string) map[string]any {
	t.Helper()
	for _, ev := range events {
		if ev.Action == action {
			return ev.Args
		}
	}
	t.Fatalf("missing workflow step action %s in %+v", action, events)
	return nil
}

func newDeployMockWithSupportZones(cfg deployMockConfig) *mockExecutorFn {
	base := newDeployMock(cfg)
	return &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		if action == "DescribeCompShareSupportZone" {
			return map[string]any{"ZoneInfo": []any{
				map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(10027), "Describe": "华北二A", "IsPod": false},
				map[string]any{"Zone": "cn-sh2-02", "Region": "cn-sh2", "RegionId": float64(3002), "ZoneId": float64(8200), "Describe": "上海二B", "IsPod": false},
				map[string]any{"Zone": "cn-bj2-03", "Region": "cn-bj2", "RegionId": float64(3003), "ZoneId": float64(5001), "Describe": "华北一C", "IsPod": false},
			}}, nil
		}
		return base.fn(action, args)
	}}
}
