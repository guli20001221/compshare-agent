package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/intent"
)

// dispatch_agent_skill_test.go pins the P2b agent-arm dispatch seam. The seam
// (dispatchAgentSkill + agentArmSkillForIntent) replaced the hardcoded
// `if Intent == IntentDeployModel { tryDeployModel }` branch; these tests prove
// it is BYTE-STABLE wiring (delegates without altering output), captures only
// agent-tier intents, and keeps the table locked to the registry + the saga
// skillID so the four copies of "deploy_model" can never silently drift.

// TestDispatchAgentSkill_RoutesDeployModelByteStable proves the seam is pure
// wiring: routing IntentDeployModel through dispatchAgentSkill yields the EXACT
// reply that calling tryDeployModel directly does. Two engines with byte-identical
// fakes (same mock-LLM script, same executor, same confirm) run the two paths and
// their replies are compared — if the seam altered any LLM input, saga param, or
// render, the strings would diverge.
func TestDispatchAgentSkill_RoutesDeployModelByteStable(t *testing.T) {
	withFastPoll(t, 5)

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

	dispatch := plannerDispatchResult{result: intent.PlannerResult{Plan: intent.Plan{Intent: intent.IntentResourceInfo}}}
	reply, handled := eng.dispatchAgentSkill(context.Background(), dispatch, "我有哪些实例", noopStep)

	assert.False(t, handled, "a non-agent intent must fall through, not be captured by the agent seam")
	assert.Empty(t, reply)
	assert.Empty(t, exec.calls, "an unmapped intent must not reach any tool")
}

// TestAgentArmSkillForIntent_BoundToRegistry locks every table value to a skill
// that actually exists in the generated registry, so deleting/renaming a skill (or
// a typo in the table) fails CI instead of falling through at runtime — critical
// because IntentDeployModel has no ReAct tool subset, so a fallthrough would reach
// a broken ReAct loop, not a graceful degrade.
func TestAgentArmSkillForIntent_BoundToRegistry(t *testing.T) {
	require.NotEmpty(t, agentArmSkillForIntent)
	for it, skillName := range agentArmSkillForIntent {
		skill, ok := findGeneratedSkill(skillName)
		require.Truef(t, ok, "intent %q maps to skill %q which is absent from the generated registry", it, skillName)
		assert.Equalf(t, skillName, skill.Name, "registry skill Name must equal the table value for intent %q", it)
	}
	assert.Equal(t, "deploy_model", agentArmSkillForIntent[intent.IntentDeployModel])
}

// TestAgentArmSkillForIntent_MatchesSagaSkillID locks the table value to the
// skillID the deploy arm actually stamps on its saga StepTraces. deploy_model.go
// hardcodes "deploy_model" as the saga skillID; if that literal ever drifts from
// the table (the "fourth copy of the same string" risk), this test fails.
func TestAgentArmSkillForIntent_MatchesSagaSkillID(t *testing.T) {
	withFastPoll(t, 5)
	exec := newDeployMock(deployMockConfig{capacityEnough: true, instanceStates: []string{"Running"}})
	eng := newDeployEngine(deployMatchJSON, exec, func(string, map[string]any) bool { return true })
	sink := &sagaFakeSink{}
	eng.SetStepSink(sink)

	_, handled := eng.dispatchAgentSkill(context.Background(), deployDispatch(), "帮我部署 Qwen2.5-7B", noopStep)
	require.True(t, handled)

	require.NotEmpty(t, sink.steps, "the deploy saga must emit step traces")
	assert.Equal(t, agentArmSkillForIntent[intent.IntentDeployModel], sink.steps[0].SkillID,
		"saga StepTrace SkillID must equal the agent-arm table value")
}
