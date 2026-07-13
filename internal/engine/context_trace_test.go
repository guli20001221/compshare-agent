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

// The most common complaint about this agent is 「它忘了我刚说的话」. Every trace in this
// system records what the agent DID; none recorded what it KNEW. Those are different
// bugs, they need opposite fixes, and they produce an identical reply:
//
//	"it had the context and ignored it"  -> fix the prompt
//	"it was never given the context"     -> fix the plumbing
//
// It turns out to be the second, and it was invisible. The ReAct loop carries
// maxHistoryMessages (120) of conversation. The intent ROUTER runs FIRST, decides which
// path the turn takes at all, and — per callPlannerOnce's own comment — has the
// transcript WITHHELD from its prompt (PR1 hotfix 2026-05-28: emitting PriorText grew
// multi-turn input until ds-v4-flash stopped returning schema-valid JSON).
//
// So the router is effectively stateless about the conversation. No amount of widening
// the loop's history rescues a turn the router already misrouted on its blind view —
// which is why an A/B of agent-loop vs fast-path measured NO change in amnesia (38.1% vs
// 34.9%): both arms share the same blind router. Two days of work went into widening the
// wrong component. This test pins the asymmetry so nobody rediscovers it that way again.
func TestRouterAndLoopSeeDifferentContext(t *testing.T) {
	var traces []observability.ContextTrace

	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{
		{Content: "第一轮回答"},
		{Content: "第二轮回答"},
	}}, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	eng.SetContextTraceObserver(func(tr observability.ContextTrace) { traces = append(traces, tr) })
	// EnabledIntents must name an ADMITTED intent or BuildIntentPlannerMaps drops it and
	// plannerDispatchEnabled() stays false, so the router never runs at all. The planner
	// then returns `unknown`, which falls through to ReAct — the router still RAN, which
	// is the only thing this test needs it to do.
	eng.SetIntentPlanner(&scriptedIntentPlanner{results: []intent.IntentRouterResult{
		{Plan: intent.IntentRoute{SchemaVersion: intent.SchemaVersion, Intent: intent.IntentUnknown, Confidence: 0.9}},
		{Plan: intent.IntentRoute{SchemaVersion: intent.SchemaVersion, Intent: intent.IntentUnknown, Confidence: 0.9}},
	}}, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentPricingQuery}})

	_, err := eng.Chat(context.Background(), "我的实例初始化失败了", noopStep)
	require.NoError(t, err)
	_, err = eng.Chat(context.Background(), "我删了再部署", noopStep)
	require.NoError(t, err)

	require.GreaterOrEqual(t, len(traces), 2, "the router ran but emitted no context trace")
	second := traces[len(traces)-1]

	// The LOOP has the conversation: by turn two it holds the system prompt, both user
	// turns, and the first reply.
	assert.Greater(t, second.LoopMessages, 3,
		"the ReAct loop should be carrying the conversation by turn 2, got %d messages",
		second.LoopMessages)

	// The ROUTER does not. This is the finding. PriorText is assembled, handed to the
	// router, and then dropped by buildUserPrompt before the model ever sees it.
	//
	// If someone wires it back in, this assertion fails — and that failure is CORRECT.
	// It means the amnesia fix landed, and it is also precisely what caused the 2026-05-28
	// schema-validity avalanche, so planner schema_valid must be re-checked on real
	// multi-turn traffic in the same change. Do not delete this to get green.
	assert.False(t, second.RouterPriorInPrompt,
		"the conversation now reaches the router's prompt — re-verify planner schema_valid "+
			"on real multi-turn traffic before accepting this (see PR1 hotfix 2026-05-28)")
}
