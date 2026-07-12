package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SessionState.LastIntent is the ENTIRE memory the intent router has. The transcript is
// withheld from its prompt (PR1 hotfix 2026-05-28), so if LastIntent is empty the router
// classifies a follow-up as though it were the opening line of the conversation.
//
// It used to be written in exactly one place — recordLastIntentFromPlan, inside
// `if handled.Status == HandlerStatusHandled`, i.e. only when a DIRECT-DISPATCH handler
// resolved the turn. And defaultRouteIntents() (cmd/trace.go) is:
//
//	resource, monitor, billing_account_unsupported, gpu_specs, stock, pricing,
//	refund_estimate, image_tag_catalog, model_repository, image_list, net_accelerator
//
// diagnosis is not in it. knowledge_qa is not in it. Both go to ReAct, both come back
// fallback_ineligible, and neither could EVER be "confirmed by a fully-dispatched handler
// reply" — so neither was ever recorded, no matter how many turns the user spent there.
//
// They are the two biggest intents in real traffic (659 knowledge_qa + 260 diagnosis of
// 1280 follow-up turns). A live replay of production sessions shows a conversation running
// FIVE consecutive diagnosis turns with router_has_last_intent=false on every single one.
// The router's only memory was structurally unable to hold the two conversations users
// actually have — which is why a user mid-troubleshoot pasting a stack trace gets told,
// in effect, "I don't handle that".
func TestARouterOnlyIntentIsStillRememberedForTheNextTurn(t *testing.T) {
	var traces []observability.ContextTrace

	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{
		{Content: "看起来是初始化失败，请把报错贴给我"},
		{Content: "这段 traceback 说明缺少 CUDA 驱动"},
	}}, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	// The HTTP path hydrates session state per turn (handleChat -> ClearSessionState ->
	// SetSessionState). Without this the engine is un-hydrated and LastIntent is never
	// written at all — a state production is never in, so a test that skipped it would be
	// asserting against fiction.
	eng.SetSessionState(SessionState{}, 1)
	eng.SetContextTraceObserver(func(tr observability.ContextTrace) { traces = append(traces, tr) })

	// The router confidently classifies BOTH turns as diagnosis. EnabledIntents contains
	// only pricing_query — mirroring production, where diagnosis is NOT a direct-dispatch
	// intent, so the turn is routed and then falls through to ReAct.
	eng.SetIntentPlanner(&scriptedIntentPlanner{results: []intent.IntentRouterResult{
		{Plan: intent.IntentRoute{SchemaVersion: intent.SchemaVersion, Intent: intent.IntentDiagnosis, Confidence: 0.9}},
		{Plan: intent.IntentRoute{SchemaVersion: intent.SchemaVersion, Intent: intent.IntentDiagnosis, Confidence: 0.9}},
	}}, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentPricingQuery}})

	_, err := eng.Chat(context.Background(), "我的实例初始化失败了", noopStep)
	require.NoError(t, err)

	// Turn 2 is the real user behaviour: paste the evidence the agent just asked for.
	// On its own text this is meaningless; it is ONLY interpretable as a continuation.
	_, err = eng.Chat(context.Background(), "Traceback (most recent call last):\n  File \"/root/app.py\"", noopStep)
	require.NoError(t, err)

	require.GreaterOrEqual(t, len(traces), 2, "the router ran twice but did not emit two context traces")

	assert.False(t, traces[0].RouterHasLastIntent,
		"turn 1 opens the conversation — there is nothing to remember yet")

	assert.True(t, traces[1].RouterHasLastIntent,
		"turn 2's router must know a diagnosis is already in progress. If this is false, the "+
			"user's pasted traceback is classified as if it were the first thing they ever said, "+
			"and `unknown` is the honest answer to it")

	assert.Equal(t, string(intent.IntentDiagnosis), eng.sessionState.LastIntent,
		"the remembered value must be the exact RuntimeIntents() string (session_state.go vocabulary contract)")
}

// The refusal in rememberLastIntentForRouter is load-bearing, not defensive noise. When a
// turn's plan is garbage, the conversation has NOT changed topic — so the previous topic
// must survive. Overwriting it with `unknown` would erase the memory on precisely the
// turns that need it most: the ones the router already failed to understand.
func TestAFailedTurnDoesNotEraseWhatTheConversationWasAbout(t *testing.T) {
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{
		{Content: "先看看是不是驱动问题"},
		{Content: "抱歉，没太理解"},
	}}, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	eng.SetSessionState(SessionState{}, 1)
	eng.SetIntentPlanner(&scriptedIntentPlanner{results: []intent.IntentRouterResult{
		{Plan: intent.IntentRoute{SchemaVersion: intent.SchemaVersion, Intent: intent.IntentDiagnosis, Confidence: 0.9}},
		// The router falls over on the follow-up and emits its unknown fallback.
		{Plan: intent.IntentRoute{SchemaVersion: intent.SchemaVersion, Intent: intent.IntentUnknown, Confidence: 0.9}},
	}}, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentPricingQuery}})

	_, err := eng.Chat(context.Background(), "GPU 掉卡了", noopStep)
	require.NoError(t, err)
	require.Equal(t, string(intent.IntentDiagnosis), eng.sessionState.LastIntent)

	_, err = eng.Chat(context.Background(), "嗯嗯", noopStep)
	require.NoError(t, err)

	assert.Equal(t, string(intent.IntentDiagnosis), eng.sessionState.LastIntent,
		"one turn the router could not read must not wipe the conversation's topic — "+
			"the NEXT turn still needs to know a diagnosis is in flight")
}
