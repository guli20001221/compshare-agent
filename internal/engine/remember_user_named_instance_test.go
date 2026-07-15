package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// threeInstances is the shape that makes the bug possible: more than one machine, so no
// single-host shortcut can rescue the turn (engine.go singleRegistryInstance), and the
// referent has to come from what the user actually said.
func threeInstances() *mockExecutor {
	return &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"TotalCount": 3.0,
			"UHostSet": []any{
				map[string]any{
					"UHostId": "uhost-1exampleaa01", "Name": "训练机A", "State": "Running",
					"Zone": "cn-wlcb-01", "GpuType": "4090", "GPU": 1.0, "CPU": 16.0, "Memory": 65536.0,
				},
				map[string]any{
					"UHostId": "uhost-1exampleaa02", "Name": "推理机B", "State": "Running",
					"Zone": "cn-wlcb-01", "GpuType": "4090", "GPU": 1.0, "CPU": 16.0, "Memory": 65536.0,
				},
				map[string]any{
					"UHostId": "uhost-1exampleaa03", "Name": "备用机C", "State": "Stopped",
					"Zone": "cn-bj2-03", "GpuType": "A100", "GPU": 2.0, "CPU": 32.0, "Memory": 131072.0,
				},
			},
		},
	}}
}

// diagnosisEngine wires the production shape: the router classifies diagnosis confidently,
// diagnosis is NOT a direct-dispatch intent (EnabledIntents deliberately excludes it), so the
// turn routes and then falls through to ReAct — which is the lane with no handler to record
// anything. The session is hydrated because the HTTP path hydrates per turn; un-hydrated,
// recordSelectedInstanceID is a no-op and the test would be asserting against fiction.
func diagnosisEngine(t *testing.T, turns int) (*Engine, *scriptedIntentPlanner, *mockExecutor) {
	t.Helper()
	responses := make([]llm.ChatResponse, 0, turns)
	plans := make([]intent.IntentRouterResult, 0, turns)
	for i := 0; i < turns; i++ {
		responses = append(responses, llm.ChatResponse{Content: "先看看驱动和端口。"})
		plans = append(plans, intent.IntentRouterResult{Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentDiagnosis,
			Confidence:    0.9,
		}})
	}
	exec := threeInstances()
	eng := NewWithDeps(&mockLLM{responses: responses}, exec, nil)
	eng.InitWithContext("test user")
	eng.SetSessionState(SessionState{}, 1)
	planner := &scriptedIntentPlanner{results: plans}
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentPricingQuery},
	})
	return eng, planner, exec
}

// THE WIRING GATE. Everything else in this file is a unit test of a helper; this is the one
// that fails if the helper is correct but nobody calls it.
//
// It drives the real Chat() twice, exactly as production does. Turn 1 is a diagnosis in which
// the user NAMES their machine. Turn 2 is the follow-up that carries no identifier at all —
// the one that, in the 06-26..07-09 production corpus, got 「请问是哪台实例出了问题？」 back for a
// machine the user had already typed out (6 of 22 such turns; 5 of those user-typed).
//
// The assertion is not on the reply text — the reply is the model's and a test must not pin
// it. It is on what the NEXT turn is handed: the router's LastSelectedInstanceID and the
// system prompt's bound instance. That is the consumption property. Remembering is only worth
// anything if the next turn can read it.
func TestDiagnosisRemembersTheMachineTheUserNamed(t *testing.T) {
	eng, planner, _ := diagnosisEngine(t, 2)

	_, err := eng.Chat(context.Background(), "我的 uhost-1exampleaa02 SSH 连不上了", noopStep)
	require.NoError(t, err)

	require.Equal(t, "uhost-1exampleaa02", eng.sessionState.SelectedInstanceID,
		"the user typed the id in their own words on a lane with no direct-dispatch handler; "+
			"nothing else in the turn will write it down")
	assert.Equal(t, "推理机B", eng.sessionState.SelectedInstanceName)
	assert.Equal(t, SelectedInstanceSourceUser, eng.sessionState.SelectedInstanceSource,
		"the user named it — recording it as merely 'observed' would be a lie in the data model")

	// The follow-up names nothing. It is interpretable ONLY as a continuation.
	_, err = eng.Chat(context.Background(), "还是不行，怎么办", noopStep)
	require.NoError(t, err)

	require.Len(t, planner.calls, 2)
	assert.Equal(t, "uhost-1exampleaa02", planner.calls[1].LastSelectedInstanceID,
		"turn 2's router must know which machine is under discussion. If this is empty, the "+
			"follow-up is classified as if it were the first thing the user ever said")

	require.NotEmpty(t, eng.messages)
	assert.Contains(t, eng.messages[0].Content, "uhost-1exampleaa02",
		"and the ReAct loop must SEE it — 「当前会话已选实例」 in the system prompt is what stops "+
			"the model asking the user to identify a machine they already named")
}

// The referent may be a NAME, not an id. Same evidence, same standard: the user said it.
func TestDiagnosisRemembersTheMachineTheUserNamedByName(t *testing.T) {
	eng, _, _ := diagnosisEngine(t, 1)

	_, err := eng.Chat(context.Background(), "备用机C 上的 GPU 掉卡了", noopStep)
	require.NoError(t, err)

	assert.Equal(t, "uhost-1exampleaa03", eng.sessionState.SelectedInstanceID)
	assert.Equal(t, SelectedInstanceSourceUser, eng.sessionState.SelectedInstanceSource)
}

func TestDiagnosisDoesNotTrustInstanceNameInsideAnotherWord(t *testing.T) {
	for _, tc := range []struct {
		name, message string
	}{
		{name: "test", message: "pytest"},
		{name: "host", message: "ghost"},
		{name: "a", message: "data"},
		{name: "机", message: "机器坏了"},
		{name: "测试", message: "测试环境异常"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
			eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
			snapshot := entity.RegistrySnapshot{Instances: map[string]entity.InstanceSnapshot{
				"uhost-target": {UHostId: "uhost-target", Name: tc.name},
			}}
			eng.rememberUserNamedInstance(tc.message, snapshot)
			assert.Empty(t, eng.sessionState.SelectedInstanceID)
			eng.selectedInstanceIDAtTurnStart = eng.sessionState.SelectedInstanceID
			eng.selectedInstanceSourceAtTurnStart = eng.sessionState.SelectedInstanceSource
			eng.lastUserMsg = "关掉它"
			assert.False(t, eng.workflowTargetIsTrusted("StopInstanceWorkflow", "uhost-target", false),
				"an ordinary substring on turn one must not authorize a write on turn two")
		})
	}
}

// THE PHANTOM-SELECTION GATE. This is the negative control the whole design hangs on, and it
// is the reason the binding is derived from the user's literal text rather than from anything
// the model produced.
//
// The user reports a symptom and names NOTHING. Three machines exist. The correct behaviour is
// to have no bound instance and let the turn ask. If this ever goes green with an id in it,
// something has started ELECTING a target — which is the exact P1 (PR #409-412) that
// workflowTargetIsTrusted exists to prevent, and no amount of "but it's usually right" makes a
// self-elected machine safe to hand a stop/reboot.
func TestASymptomWithNoNamedMachineBindsNothing(t *testing.T) {
	eng, _, _ := diagnosisEngine(t, 1)

	_, err := eng.Chat(context.Background(), "实例突然连不上了，怎么排查", noopStep)
	require.NoError(t, err)

	assert.Empty(t, eng.sessionState.SelectedInstanceID,
		"three machines, none named: there is no referent. Picking one is hallucination, not memory")
}

// Two machines in one breath is ambiguity, not evidence.
func TestNamingTwoMachinesBindsNeither(t *testing.T) {
	eng, _, _ := diagnosisEngine(t, 1)

	_, err := eng.Chat(context.Background(),
		"uhost-1exampleaa01 和 uhost-1exampleaa03 都连不上", noopStep)
	require.NoError(t, err)

	assert.Empty(t, eng.sessionState.SelectedInstanceID,
		"two referents is not one referent; silently binding the first would be a coin flip on "+
			"which machine a later 「关掉它」 hits")
}

// The `len(unresolved) > 0` guard, on the only input that isolates it.
//
// Written after a mutation showed the guard was DECORATIVE against the plain-typo test below:
// on a bare typo `hits` is empty anyway, so the empty-hits path caught it and the guard never
// had to fire. It takes a message carrying BOTH a machine we can resolve AND one we cannot to
// make the guard the only thing standing.
//
// Resolving one of two named machines and binding it is a coin flip on which machine a later
// 「关掉它」 lands on. It is also exactly what a TRUNCATED registry looks like from the inside
// (entity.CanAssertAbsence): the second machine may be perfectly real and simply absent from
// the page we fetched. "I could only see one of the two you named" is not "you meant that one".
func TestOneMachineWeCanSeeAndOneWeCannotBindsNeither(t *testing.T) {
	eng, _, _ := diagnosisEngine(t, 1)

	_, err := eng.Chat(context.Background(),
		"uhost-1exampleaa02 和 uhost-1exampleaa99 都连不上", noopStep)
	require.NoError(t, err)

	assert.Empty(t, eng.sessionState.SelectedInstanceID,
		"one resolvable + one unresolvable is still TWO referents. Binding the half we happened "+
			"to be able to see is a coin flip, not memory")
}

// An id-shaped token this account does not own must bind nothing — NOT the nearest match. A
// typo that silently re-pointed the session at a different machine would be worse than asking.
func TestAnIDShapedTypoBindsNothing(t *testing.T) {
	eng, _, _ := diagnosisEngine(t, 1)

	_, err := eng.Chat(context.Background(), "uhost-1exampleaa99 起不来", noopStep)
	require.NoError(t, err)

	assert.Empty(t, eng.sessionState.SelectedInstanceID,
		"the registry resolved nothing for that token; a near-miss must not be silently corrected "+
			"into some other machine the user never mentioned")
}

// THE PROVENANCE GATE. TargetRef.Source is a field the MODEL fills in — the router can simply
// assert "user_text" for an id the user never typed, which is precisely why validateProvenance
// exists. So the binding must not be read off the plan.
//
// Here the router hands back a confident, schema-valid plan naming a REAL instance that the
// user never mentioned. If rememberUserNamedInstance ever starts trusting plan.Slots.TargetRefs,
// this goes red — and a model-invented target would be one 「关掉它」 away from a stop workflow.
func TestARouterClaimedTargetTheUserNeverTypedBindsNothing(t *testing.T) {
	eng, _, _ := diagnosisEngine(t, 1)
	eng.SetIntentPlanner(&scriptedIntentPlanner{results: []intent.IntentRouterResult{
		{Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentDiagnosis,
			Confidence:    0.95,
			Slots: intent.Slots{TargetRefs: []intent.TargetRef{{
				Type:  intent.TargetRefUHostIDUserInput,
				Value: "uhost-1exampleaa01",
				// The model asserts clean provenance. It is lying: the id is nowhere in the
				// user's text below.
				Source:     intent.SourceUserText,
				SourceSpan: "uhost-1exampleaa01",
			}}},
		}},
	}}, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentPricingQuery}})

	_, err := eng.Chat(context.Background(), "机器有点问题", noopStep)
	require.NoError(t, err)

	assert.Empty(t, eng.sessionState.SelectedInstanceID,
		"the id came from the MODEL, not the user. A binding sourced from model output is the "+
			"phantom-instance bug wearing a provenance field")
}

// The registry is COLD on an early HTTP turn (that path skips engine.Init()), and entity derives
// the id prefixes it scans for from the instances it holds — so an empty registry cannot even
// recognise "uhost-…" as an instance reference. Without going to look, the user names their
// machine and we see nothing.
//
// Mutation: make diagnosisResolutionSnapshot return `cached` unconditionally and this is the
// only test in the file that goes red — the others all run after some other code path has
// happened to warm the registry.
func TestAColdRegistryGoesAndLooksInsteadOfShrugging(t *testing.T) {
	eng, _, exec := diagnosisEngine(t, 1)
	require.Empty(t, eng.RegistrySnapshot().Instances,
		"precondition: nothing has listed the account yet, exactly as on an early HTTP turn")

	_, err := eng.Chat(context.Background(), "我的 uhost-1exampleaa01 起不来", noopStep)
	require.NoError(t, err)

	assert.Contains(t, exec.calls, "DescribeCompShareInstance",
		"a registry that cannot assert absence has no standing to say it has never heard of the "+
			"machine the user just named — it has to go and look")
	assert.Equal(t, "uhost-1exampleaa01", eng.sessionState.SelectedInstanceID)
}
