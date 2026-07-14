package engine

import (
	"context"
	"testing"
	"time"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/zones"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A frame's life is decided by whether the USER moved on. It is not decided by whether the
// ROUTER understood them.
//
// These two halves are one bug wearing two faces: the keep/clear decision was wired to the
// wrong signal, so the frame was simultaneously too FRAGILE (any router wobble deleted it)
// and too STICKY (a dead deploy's workload rode into the next unrelated create). Fixing
// only the fragility would widen the stickiness window, which is why they ship together.

func liveDeployFrame() ContextFrame {
	return ContextFrame{
		Version:        1,
		Kind:           ContextFrameKindDeploy,
		Status:         ContextFrameStatusFailedRecoverable,
		Workload:       "DeepSeek R1",
		GPU:            "4090",
		Zone:           "cn-wlcb-01",
		ImagePref:      "vLLM",
		ProducedAtUnix: time.Now().Unix(),
		TTLSeconds:     ContextFrameTTLSeconds,
	}
}

func engineWithLiveDeployFrame(t *testing.T, results []intent.IntentRouterResult) *Engine {
	t.Helper()
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{
		SchemaVersion:      SessionStateSchemaCurrent,
		LastIntent:         string(intent.IntentDeployModel),
		PendingDeployModel: "DeepSeek R1",
		ContextFrame:       liveDeployFrame(),
	}, 1)
	eng.SetIntentPlanner(&scriptedIntentPlanner{results: results},
		IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentResourceInfo}})
	return eng
}

// ── Half 1: the router being UNSURE is not the user giving up ──────────────────────────

// A short, vague follow-up mid-deploy is exactly the input ds-v4-flash is least confident on.
// So the confidence gate tripped hardest on precisely the turns where continuation mattered
// most, and the turn that most needed the frame was the turn that destroyed it.
func TestTryPlannerDispatch_LowConfidenceMustNotDeleteTheUsersDeployment(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	eng := engineWithLiveDeployFrame(t, []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentUnknown,
			Slots:         intent.Slots{TargetRefs: []intent.TargetRef{}, Metrics: []intent.Metric{}},
			RequiredTools: []string{},
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.4, // under the 0.60 bar: the router is UNSURE, not informed
		},
	}})

	reply, handled := eng.tryPlannerDispatch(context.Background(), "嗯", "", noopStep, nil)

	require.False(t, handled, "an unsure router must fall through to ReAct")
	require.Empty(t, reply)

	state, _, _ := eng.SessionStateSnapshot()
	assert.Equal(t, ContextFrameKindDeploy, state.ContextFrame.Kind,
		"the router scoring 0.4 says nothing about whether the user abandoned their deployment — "+
			"deleting the frame here throws away GPU, zone, image and the failure reason, and the "+
			"continuation resolver that would have judged it never even gets the turn")
	assert.Equal(t, "DeepSeek R1", state.ContextFrame.Workload,
		"the task itself must survive, not just the frame's shell")
	assert.Equal(t, "DeepSeek R1", state.PendingDeployModel)
}

// The nastiest version: nothing about the USER changed at all. The router just failed —
// a timeout, a 5xx, an LLM-class rate-limit denial. callPlannerOnce seeds
// Fallback:true and only overwrites it on success, so one transient network blip used to
// silently delete an in-progress deployment.
func TestTryPlannerDispatch_ARouterErrorMustNotDeleteTheUsersDeployment(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	eng := engineWithLiveDeployFrame(t, []intent.IntentRouterResult{{
		Fallback: true, // the router call itself failed
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentUnknown,
			Retrieval:     intent.Retrieval{Enabled: false},
		},
	}})

	_, handled := eng.tryPlannerDispatch(context.Background(), "那用 A100 呢", "", noopStep, nil)
	require.False(t, handled)

	state, _, _ := eng.SessionStateSnapshot()
	assert.Equal(t, ContextFrameKindDeploy, state.ContextFrame.Kind,
		"a 5xx from the intent router is a fact about our infrastructure, not about the user's intent; "+
			"it must not cost them their deployment")
	assert.Equal(t, "DeepSeek R1", state.ContextFrame.Workload)
}

// The other side of the same line, and the reason the fix is "keep it" rather than "resume
// it": a sub-0.60 classification is untrusted input, and tryResumeCreateContextFrame ends in
// runDeployModel — a MUTATING saga. Surviving the wobble must not mean acting on it.
func TestTryPlannerDispatch_AnUnsureRouterMustNotDriveTheDeploySaga(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	exec := &mockExecutor{}
	eng := NewWithDeps(&mockLLM{}, exec, nil)
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		ContextFrame:  liveDeployFrame(),
	}, 1)
	eng.SetIntentPlanner(&scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentCreateInstance, // it GUESSED create...
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.4, // ...but it has no idea
		},
	}}}, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentResourceInfo}})

	_, handled := eng.tryPlannerDispatch(context.Background(), "嗯", "", noopStep, nil)

	assert.False(t, handled, "an unsure router must not be allowed to dispatch")
	assert.Empty(t, exec.calls,
		"and it must not execute anything. Keeping the frame is not the same as acting on it: "+
			"letting a 0.4-confidence guess drive the deploy saga would be a worse bug than the "+
			"one this fixes")
}

// ── Half 2: a dead deploy must not ride into the next create ───────────────────────────

// The stickiness half. applyContextContinuationDecision starts from a full copy of the old
// frame and only overwrites what the resolver named, so a failed 「部署 DeepSeek R1」 keeps
// Workload="DeepSeek R1" — and the very next 「创建一台 4090」, which the router classified
// confidently as create_instance with NO workload, is rewritten into deploy_model and handed
// to runDeployModel. The user asked for a bare GPU box and got an attempt to redeploy the
// model that had just failed on them.
func TestDropStaleDeployPayload_APureHardwareCreateDoesNotInheritADeadDeploy(t *testing.T) {
	merged := liveDeployFrame() // what the merge hands back: the dead deploy, intact
	decision := ContextContinuationDecision{Decision: ContextContinuationContinue}

	got := dropStaleDeployPayload(merged, intent.IntentCreateInstance, decision)

	assert.Empty(t, got.Workload,
		"the user said 创建一台 4090 and named no workload; a DeepSeek R1 carried over from a "+
			"previous failed deploy is not context, it is contamination — and downstream it flips "+
			"the whole turn into runDeployModel")
	assert.Empty(t, got.ImagePref, "the stale image is the specific thing users have watched get stuck")
	assert.Empty(t, got.ImageSource)
	assert.Equal(t, ContextFrameKindCreate, got.Kind,
		"with nothing deploy-shaped left, the frame IS a create — leaving Kind=deploy would keep "+
			"the caller's `Kind == deploy` branch routing it to the saga anyway")

	// The legitimately reusable half must survive: "再来一台" should still mean 4090 in 华北一.
	assert.Equal(t, "4090", got.GPU, "GPU is real continuation context, not contamination")
	assert.Equal(t, "cn-wlcb-01", got.Zone, "and so is the zone")
}

// The negative control, and the thing that keeps this from being an over-fix: when the user
// DOES name a workload this turn, it is a deploy and must stay one. A fix that strips the
// workload unconditionally would break every genuine 「用 vLLM 部署」.
func TestDropStaleDeployPayload_AWorkloadNamedThisTurnSurvives(t *testing.T) {
	merged := liveDeployFrame()
	merged.Workload = "Qwen3" // the resolver picked this up from THIS turn's message
	decision := ContextContinuationDecision{
		Decision:     ContextContinuationContinue,
		WorkloadPref: "Qwen3",
	}

	got := dropStaleDeployPayload(merged, intent.IntentCreateInstance, decision)

	assert.Equal(t, "Qwen3", got.Workload,
		"the user named a workload on THIS turn — that is a deploy, and dropping it would break "+
			"every genuine 「创建一台机器部署 Qwen3」")
	assert.Equal(t, ContextFrameKindDeploy, got.Kind)
}

// And a real deploy continuation must be untouched. The guard keys on the router's intent, so
// 「换成 A100」 mid-deploy (router: deploy_model) keeps everything it is continuing.
func TestDropStaleDeployPayload_ADeployContinuationIsUntouched(t *testing.T) {
	merged := liveDeployFrame()
	decision := ContextContinuationDecision{Decision: ContextContinuationContinue}

	got := dropStaleDeployPayload(merged, intent.IntentDeployModel, decision)

	assert.Equal(t, "DeepSeek R1", got.Workload,
		"the router says this turn IS a deploy; the workload it is continuing must survive")
	assert.Equal(t, "vLLM", got.ImagePref)
	assert.Equal(t, ContextFrameKindDeploy, got.Kind)
}

// ── The WIRING ─────────────────────────────────────────────────────────────────────────
//
// The three tests above exercise dropStaleDeployPayload as a function. Deleting its CALL SITE
// leaves every one of them green — which would make them a decorative gate over a live bug.
// This one drives the real tryResumeCreateContextFrame end to end and asserts the turn comes
// out as a plain hardware create, so the fix has to actually be plugged in.
func TestResumeCreateContextFrame_ADeadDeployDoesNotHijackTheNextHardwareCreate(t *testing.T) {
	SetContextContinuationEnabled(true)
	t.Cleanup(func() { SetContextContinuationEnabled(false) })

	var createArgs map[string]any
	exec := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "CreateCompShareInstance":
			createArgs = args
			return map[string]any{"UHostIds": []any{"uhost-1exampleaa01"}}, nil
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-1exampleaa01", "State": "Running", "GpuType": "4090",
			}}}, nil
		case "GetCompSharePrice":
			return map[string]any{"PriceDetails": []any{map[string]any{"ChargeType": "Postpay", "Price": 1.23}}}, nil
		default:
			return map[string]any{}, nil
		}
	}}

	// The deploy saga needs the LLM (image matching). Give it nothing: if the hijack happens,
	// it cannot quietly succeed and pass for a create.
	eng := NewWithDeps(&mockLLM{}, exec, okConfirm)
	eng.zoneCatalog = zones.NewCatalog(0)

	// The user said 「再开一台 4090」. They named NO workload and NO image — the resolver
	// reports a continuation with nothing deploy-shaped in it.
	eng.SetContextContinuationResolver(&fakeContextContinuationResolver{
		decision: &ContextContinuationDecision{Decision: ContextContinuationContinue, GPUPref: "4090"},
	})
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		// ...but a 「部署 DeepSeek R1」 failed here a minute ago, and its frame is still warm.
		ContextFrame: ContextFrame{
			Version:         1,
			Kind:            ContextFrameKindDeploy,
			Status:          ContextFrameStatusFailedRecoverable,
			Intent:          string(intent.IntentDeployModel),
			OriginalUserMsg: "帮我部署 DeepSeek R1",
			Workload:        "DeepSeek R1",
			ImagePref:       "vLLM",
			ImageSource:     "platform",
			GPU:             "4090",
			Zone:            "cn-bj2-03",
			ZoneLabel:       "华北一C",
			FailureReason:   "4090 暂无库存",
			ProducedAtUnix:  time.Now().Unix(),
			TTLSeconds:      ContextFrameTTLSeconds,
		},
	}, 1)

	// The router is CONFIDENT this turn is a plain create. That is the signal that must win.
	dispatch := routerDispatchResult{result: intent.IntentRouterResult{Plan: intent.IntentRoute{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentCreateInstance,
		Confidence:    0.9,
	}}}

	var steps []StepEvent
	reply, handled := eng.tryResumeCreateContextFrame(context.Background(), dispatch, "再开一台 4090",
		func(ev StepEvent) { steps = append(steps, ev) })

	require.True(t, handled, "the frame's GPU/zone are still good context — the turn should resume as a create")

	var actions []string
	var createStepArgs map[string]any
	for _, ev := range steps {
		if ev.Type != StepToolCall {
			continue
		}
		actions = append(actions, ev.Action)
		if ev.Action == "CreateInstanceWorkflow" {
			createStepArgs = ev.Args
		}
	}

	// THE ASSERTION. The turn must dispatch CreateInstanceWorkflow — a plain hardware create.
	// If the dead deploy's workload rides through the merge, the caller reads
	// `nextFrame.Workload != ""`, rewrites the intent to deploy_model, and hands the turn to
	// runDeployModel instead: the user asked for a bare 4090 and gets an attempt to redeploy
	// the model that had just failed on them.
	require.Contains(t, actions, "CreateInstanceWorkflow",
		"this must come out as a PLAIN HARDWARE CREATE, not the deploy saga\ngot steps: %v\nreply: %s",
		actions, reply)

	// The legitimately reusable half of the frame still carries: 「再开一台」 should still mean
	// a 4090 in 华北一. Read off the dispatched args, so this pins what the workflow was
	// actually asked to build.
	require.NotNil(t, createStepArgs)
	assert.Equal(t, "4090", createStepArgs["GpuType"], "GPU is real carried context and must survive")
	assert.Equal(t, "cn-bj2-03", createStepArgs["Zone"], "and so is the zone")
	_ = createArgs // the workflow's own reads are not mocked out; the dispatch is what this gate pins
}
