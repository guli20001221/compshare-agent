package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/intent"
)

// dispatch_agent_skill_test.go pins the P2b agent-handler dispatch seam. The seam
// (dispatchAgentSkill + DispatchSpec) replaced the hardcoded
// `if Intent == IntentDeployModel { tryDeployModel }` branch; these tests prove
// it is BYTE-STABLE wiring (delegates without altering output), captures only
// agent-tier intents, and keeps the table locked to the saga skillID so the
// copies of "deploy_model" can never silently drift.

// TestDispatchAgentSkill_RoutesDeployModelByteStable proves the seam is pure
// wiring: routing IntentDeployModel through dispatchAgentSkill yields the EXACT
// reply that calling tryDeployModel directly does. Two engines with byte-identical
// fakes (same mock-LLM script, same executor, same confirm) run the two paths and
// their replies are compared — if the seam altered any LLM input, saga param, or
// render, the strings would diverge.
func TestDispatchAgentSkill_RoutesDeployModelByteStable(t *testing.T) {
	execA := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	engA := newDeployEngine(deployMatchJSON, execA, func(string, map[string]any) bool { return true })
	viaSeam, handledA := engA.dispatchAgentSkill(context.Background(), deployDispatch(), "帮我部署 Qwen2.5-7B", noopStep)

	execB := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	engB := newDeployEngine(deployMatchJSON, execB, func(string, map[string]any) bool { return true })
	viaArm, handledB := engB.tryDeployModel(context.Background(), deployDispatch(), "帮我部署 Qwen2.5-7B", noopStep)

	require.True(t, handledA, "the seam must own the deploy turn")
	require.True(t, handledB)
	assert.Equal(t, viaArm, viaSeam, "dispatchAgentSkill must delegate byte-identically to tryDeployModel")
}

// TestDispatchAgentSkill_UnmappedIntentFallsThrough pins that the seam captures
// ONLY agent-tier intents: a non-agent intent returns ("", false) so the dispatch
// chain continues to the Phase-1/RAG branches unchanged. Without this, the seam
// could silently swallow an intent the old per-intent branch never touched.
func TestDispatchAgentSkill_UnmappedIntentFallsThrough(t *testing.T) {
	exec := &mockExecutorFn{fn: func(string, map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	}}
	eng := NewWithDeps(nil, exec, nil)

	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{Intent: intent.IntentResourceInfo}}}
	reply, handled := eng.dispatchAgentSkill(context.Background(), dispatch, "我有哪些实例", noopStep)

	assert.False(t, handled, "a non-agent intent must fall through, not be captured by the agent seam")
	assert.Empty(t, reply)
	assert.Empty(t, exec.calls, "an unmapped intent must not reach any tool")
}

func TestDispatchAgentSkill_IgnoresObserveOnlyPlanSkills(t *testing.T) {
	exec := &mockExecutorFn{fn: func(string, map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	}}
	eng := NewWithDeps(nil, exec, nil)

	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{
		Intent: intent.IntentResourceInfo,
		Skills: []intent.SelectedSkill{
			{Name: "deploy_model", Resolution: intent.SkillResolutionAgentArm},
		},
	}}}
	reply, handled := eng.dispatchAgentSkill(context.Background(), dispatch, "我有哪些实例", noopStep)

	assert.False(t, handled, "Plan.Skills is observe-only and must not capture dispatch")
	assert.Empty(t, reply)
	assert.Empty(t, exec.calls)
}

// TestAgentSkillForIntent_BoundToDedicatedArm locks every table value to a
// dedicated agent handler. deploy_model is a saga workflow handler, not a body-read
// skill, so it is deliberately not looked up in the generated skill registry.
func TestAgentSkillForIntent_BoundToDedicatedArm(t *testing.T) {
	seen := map[intent.Intent]string{}
	for _, it := range intent.AllIntents() {
		skillName := specForIntent(it).AgentSkillName
		if skillName == "" {
			continue
		}
		seen[it] = skillName
		switch skillName {
		case "deploy_model":
			assert.Equal(t, intent.IntentDeployModel, it)
		case "create_instance":
			assert.Equal(t, intent.IntentCreateInstance, it)
		default:
			t.Fatalf("intent %q maps to unknown agent handler %q", it, skillName)
		}
	}
	require.NotEmpty(t, seen)
	assert.Equal(t, "deploy_model", seen[intent.IntentDeployModel])
	assert.Equal(t, "create_instance", seen[intent.IntentCreateInstance])
}

func TestAgentSkillForIntent_MatchesCodeDerivedPlanSkills(t *testing.T) {
	for _, it := range intent.AllIntents() {
		skillName := specForIntent(it).AgentSkillName
		if skillName == "" {
			continue
		}
		derived := intent.DeriveSelectedSkills(intent.IntentRoute{Intent: it})
		require.Lenf(t, derived, 1, "intent %q should derive exactly one agent-handler skill", it)
		assert.Equalf(t, skillName, derived[0].Name, "intent %q agent-handler skill drifted from Plan.Skills projection", it)
		assert.Equal(t, intent.SkillResolutionAgentArm, derived[0].Resolution)
	}
}

// TestAgentSkillForIntent_MatchesSagaSkillID locks the table value to the
// skillID the deploy handler actually stamps on its saga StepTraces. deploy_model.go
// hardcodes "deploy_model" as the saga skillID; if that literal ever drifts from
// the table (the "fourth copy of the same string" risk), this test fails.
func TestAgentSkillForIntent_MatchesSagaSkillID(t *testing.T) {
	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { return true })
	sink := &sagaFakeSink{}
	eng.SetStepSink(sink)

	_, handled := eng.dispatchAgentSkill(context.Background(), deployDispatch(), "帮我部署 Qwen2.5-7B", noopStep)
	require.True(t, handled)

	require.NotEmpty(t, sink.steps, "the deploy saga must emit step traces")
	assert.Equal(t, specForIntent(intent.IntentDeployModel).AgentSkillName, sink.steps[0].SkillID,
		"saga StepTrace SkillID must equal the agent-handler table value")
}
