package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
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

func TestDispatchAgentSkill_CreateInstanceFlagOnSpecOnlyUsesGuidedCreateWithoutImageMatch(t *testing.T) {
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

func TestDispatchAgentSkill_CreateInstancePriceQuestionDoesNotOpenCreateCard(t *testing.T) {
	SetUnifiedCreateEnabled(true)
	t.Cleanup(func() { SetUnifiedCreateEnabled(false) })

	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { return true })

	reply, handled := eng.dispatchAgentSkill(context.Background(), createDispatch(), "开一台4090多少钱", noopStep)

	assert.True(t, handled)
	assert.Contains(t, reply, "不会直接创建")
	assert.Equal(t, 0, countCalls(exec.calls, "CreateCompShareInstance"))
}

func TestDispatchAgentSkill_CreateInstanceHowToDoesNotOpenCreateCard(t *testing.T) {
	SetUnifiedCreateEnabled(true)
	t.Cleanup(func() { SetUnifiedCreateEnabled(false) })

	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { return true })

	reply, handled := eng.dispatchAgentSkill(context.Background(), createDispatch(), "怎么部署一台4090跑Qwen", noopStep)

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
