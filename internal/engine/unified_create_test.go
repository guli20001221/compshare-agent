package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/zones"
)

func createDispatch() routerDispatchResult {
	return routerDispatchResult{
		result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentCreateInstance}},
	}
}

func TestUnifiedCreateFlag_DefaultOff(t *testing.T) {
	SetUnifiedCreateEnabled(false)
	assert.False(t, UnifiedCreateEnabled())
}

func TestDispatchAgentSkill_CreateInstanceFlagOffFallsThrough(t *testing.T) {
	SetUnifiedCreateEnabled(false)
	exec := newDeployMock(deployMockConfig{capacityEnough: true})
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { return true })

	reply, handled := eng.dispatchAgentSkill(context.Background(), createDispatch(), "创建一台4090", noopStep)

	assert.False(t, handled)
	assert.Empty(t, reply)
	assert.Empty(t, exec.calls)
}

func TestDirectHardwareCreate_CreateInstanceIntentUsesRescueWhenUnifiedCreateOff(t *testing.T) {
	SetUnifiedCreateEnabled(false)
	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { return false })

	reply, handled := eng.tryDirectHardwareCreate(context.Background(), createDispatch(), "帮我创建一台4090", noopStep)

	require.True(t, handled)
	assert.Contains(t, reply, "创建实例")
	assert.Equal(t, 0, countCalls(exec.calls, "CreateCompShareInstance"), "confirm=false path must not create")
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
	t.Cleanup(func() { SetUnifiedCreateEnabled(false) })

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
			t.Cleanup(func() { SetUnifiedCreateEnabled(false) })

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

func TestDispatchAgentSkill_CreateInstanceSpecOnlyResolvesZonePreference(t *testing.T) {
	SetUnifiedCreateEnabled(true)
	t.Cleanup(func() { SetUnifiedCreateEnabled(false) })

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
	t.Cleanup(func() { SetUnifiedCreateEnabled(false) })

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
	t.Cleanup(func() { SetUnifiedCreateEnabled(false) })

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
	t.Cleanup(func() { SetUnifiedCreateEnabled(false) })

	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { return true })

	reply, handled := eng.dispatchAgentSkill(context.Background(), createDispatch(), "   ", noopStep)

	assert.True(t, handled)
	assert.Contains(t, reply, "不会直接创建")
	assert.Equal(t, 0, countCalls(exec.calls, "CreateCompShareInstance"))
}

func TestDispatchAgentSkill_UnifiedCreateMixedWorkloadUsesDeployMatcherWithPinnedGPU(t *testing.T) {
	SetUnifiedCreateEnabled(true)
	t.Cleanup(func() {
		SetUnifiedCreateEnabled(false)
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
