package engine

// Tests pinning the PR #61 design invariant: planner.HardBlockHint is
// advisory only — it ships to RouterTrace.HardBlockHint for observability
// but does NOT participate in cutover routing.
//
// Sibling tests already verify the actually-executed hard-block sources
// produce the correct EngineHardBlockTrace.TriggeredBy value:
//   - keyword preblock        → jailbreak/off-topic preblock tests
//   - planner_intent dispatch → TestPlannerAccountBillingUnsupportedReturnsFixedReplyWithoutTools
//   - post_llm cited contract → TestStage2BRetrievalCommonPredicateFallbacksDoNotCallRetriever
//
// The two tests here cover the inverse: HardBlockHint=true is silent unless
// another stage independently refuses. No "both" attribution is possible
// because the short-circuited stages are unobservable (see memory
// feedback_attribution_observable_only).

import (
	"context"
	"fmt"
	"testing"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/refusal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCommonPlannerCandidateStatus_HardBlockHintAdvisoryOnly pins the core
// behavior change at unit level: HardBlockHint=true alone must produce
// RouteStatusDispatched, not RouteStatusFallbackHardBlockHint. The
// previous routing branch is gone — HardBlockHint is now observability
// only.
func TestCommonPlannerCandidateStatus_HardBlockHintAdvisoryOnly(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	cases := []struct {
		name       string
		result     intent.IntentRouterResult
		wantStatus intent.RouteStatus
		wantOK     bool
	}{
		{
			name: "hint_true_high_confidence_dispatches",
			result: intent.IntentRouterResult{Plan: intent.IntentRoute{
				SchemaVersion: intent.SchemaVersion,
				Intent:        intent.IntentBillingAccountUnsupported,
				HardBlockHint: true,
				Retrieval:     intent.Retrieval{Enabled: false},
				Confidence:    0.9,
			}},
			wantStatus: intent.RouteStatusDispatched,
			wantOK:     true,
		},
		{
			name: "hint_false_high_confidence_dispatches",
			result: intent.IntentRouterResult{Plan: intent.IntentRoute{
				SchemaVersion: intent.SchemaVersion,
				Intent:        intent.IntentBillingAccountUnsupported,
				HardBlockHint: false,
				Retrieval:     intent.Retrieval{Enabled: false},
				Confidence:    0.9,
			}},
			wantStatus: intent.RouteStatusDispatched,
			wantOK:     true,
		},
		{
			name: "hint_true_low_confidence_still_falls_back",
			result: intent.IntentRouterResult{Plan: intent.IntentRoute{
				SchemaVersion: intent.SchemaVersion,
				Intent:        intent.IntentBillingAccountUnsupported,
				HardBlockHint: true,
				Retrieval:     intent.Retrieval{Enabled: false},
				Confidence:    0.3,
			}},
			// HardBlockHint does NOT participate, but low confidence still
			// triggers fallback (the legitimate non-HardBlockHint reason).
			wantStatus: intent.RouteStatusFallbackLowConfidence,
			wantOK:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, ok := eng.commonPlannerCandidateStatus(tc.result)
			assert.Equal(t, tc.wantStatus, status,
				"PR #61: HardBlockHint must be advisory only — see fallback_hard_block_hint deletion")
			assert.Equal(t, tc.wantOK, ok)
		})
	}
}

// TestPlanner_HardBlockHint_KeywordMiss_NoRefusal covers the user-visible
// concern motivating PR #61: pre-fix, HardBlockHint=true pushed queries off
// the cutover path into ReAct, where downstream guards sometimes refused
// and sometimes did not — so the same question could be refused or
// answered depending on jitter. Post-fix, a hint-only plan with no keyword
// match must produce a normal answer.
func TestPlanner_HardBlockHint_KeywordMiss_NoRefusal(t *testing.T) {
	plan := knowledgeQAPlan(false)
	plan.HardBlockHint = true
	plan.Confidence = 0.9
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{Plan: plan}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "normal answer"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	var hardBlocks []observability.EngineHardBlockTrace
	eng.SetHardBlockObserver(func(trace observability.EngineHardBlockTrace) {
		hardBlocks = append(hardBlocks, trace)
	})
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})

	// Neutral question — no account_billing / monitor_history keyword
	// matches; planner classifies it as knowledge_qa with HardBlockHint
	// erroneously set. Pre-PR #61: this could refuse via ReAct guard drift.
	// Post-PR #61: must produce the LLM's normal answer.
	reply, err := eng.Chat(context.Background(), "如何创建一个 GPU 实例", noopStep)
	require.NoError(t, err)

	assert.Equal(t, "normal answer", reply,
		"HardBlockHint=true without keyword match must not refuse — user behavior must be stable")
	assert.Empty(t, hardBlocks,
		"advisory HardBlockHint must not synthesize an engine_hard_block on its own")
}

func TestPlannerAccountBillingUnsupportedReturnsFixedReplyWithoutTools(t *testing.T) {
	cases := []string{
		"账号余额还剩多少",
		"本月总账单是多少",
		"消费流水在哪里查",
		"我的发票开好了吗",
		"余额可以提现吗",
	}
	for _, userMsg := range cases {
		t.Run(userMsg, func(t *testing.T) {
			planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{Plan: accountBillingUnsupportedPlan()}}}
			mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
			executor := &mockExecutor{}
			eng := NewWithDeps(mock, executor, nil)
			eng.InitWithContext("test user")
			var plannerTraces []observability.RouterTrace
			eng.SetPlannerTraceObserver(func(trace observability.RouterTrace) {
				plannerTraces = append(plannerTraces, trace)
			})
			var hardBlocks []observability.EngineHardBlockTrace
			eng.SetHardBlockObserver(func(trace observability.EngineHardBlockTrace) {
				hardBlocks = append(hardBlocks, trace)
			})
			eng.SetIntentPlanner(planner, IntentPlannerOptions{
				EnabledIntents: []intent.Intent{intent.IntentBillingAccountUnsupported},
				Model:          "deepseek-v4-flash",
			})
			onStep, steps := collectSteps()

			reply, err := eng.Chat(context.Background(), userMsg, onStep)

			require.NoError(t, err)
			assert.Equal(t, refusal.AccountBillingUnsupported, reply)
			assert.Empty(t, mock.calls, "account-level billing refusal must not fall through to ReAct")
			assert.Empty(t, executor.calls, "account-level billing refusal must not call upstream tools")
			assert.Empty(t, *steps, "account-level billing refusal should be a plain message, not a tool step")
			require.Len(t, plannerTraces, 1)
			assert.Equal(t, string(intent.RouteStatusDispatched), plannerTraces[0].RouteStatus)
			require.Len(t, hardBlocks, 1)
			assert.Equal(t, refusal.CategoryAccountBilling, hardBlocks[0].Category)
			assert.True(t, hardBlocks[0].Hit)
			assert.Equal(t, observability.HardBlockTriggerPlannerIntent, hardBlocks[0].TriggeredBy)
		})
	}
}

func TestPlannerAccountBillingUnsupportedClearsPendingResourceSelection(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{
		{Plan: phase1MonitorPlanWithoutTarget()},
		{Plan: accountBillingUnsupportedPlan()},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "should not be called"}}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": phase1MultipleInstanceDescribeResult(),
	}}
	eng := NewWithDeps(mock, executor, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentMonitorQuery, intent.IntentBillingAccountUnsupported},
		Model:          "deepseek-v4-flash",
	})
	var hardBlocks []observability.EngineHardBlockTrace
	eng.SetHardBlockObserver(func(trace observability.EngineHardBlockTrace) {
		hardBlocks = append(hardBlocks, trace)
	})

	firstReply, err := eng.Chat(context.Background(), "CPU 高怎么办", noopStep)
	require.NoError(t, err)
	require.NotNil(t, eng.pendingResourceSelection)
	assert.Contains(t, firstReply, "uhost-select-001")

	secondReply, err := eng.Chat(context.Background(), "账号余额还剩多少", noopStep)
	require.NoError(t, err)

	assert.Equal(t, refusal.AccountBillingUnsupported, secondReply)
	assert.Nil(t, eng.pendingResourceSelection)
	assert.Equal(t, []string{"DescribeCompShareInstance"}, executor.calls)
	assert.Empty(t, mock.calls)
	require.Len(t, hardBlocks, 1)
	assert.Equal(t, refusal.CategoryAccountBilling, hardBlocks[0].Category)
	assert.Equal(t, observability.HardBlockTriggerPlannerIntent, hardBlocks[0].TriggeredBy)
}

func TestPlannerAccountBillingUnsupportedWithUHostPrefixClearsPendingResourceSelection(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{Plan: accountBillingUnsupportedPlan()}}}
	executor := &mockExecutor{}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.InitWithContext("test user")
	candidates := []entity.InstanceSnapshot{
		testInstance("uhost-select-001", "select-a", "Running"),
		testInstance("uhost-select-002", "select-b", "Running"),
	}
	eng.pendingResourceSelection = &pendingResourceSelection{
		originalUserMsg: "CPU 高怎么办",
		plan:            phase1MonitorPlanWithoutTarget(),
		snapshot:        testSnapshotWithInstances(candidates...),
		candidates:      candidates,
		createdTurn:     eng.userTurn,
	}
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentBillingAccountUnsupported},
		Model:          "deepseek-v4-flash",
	})

	reply, err := eng.Chat(context.Background(), "uhost-select-002 的账号余额还剩多少", noopStep)

	require.NoError(t, err)
	assert.Equal(t, refusal.AccountBillingUnsupported, reply)
	assert.Nil(t, eng.pendingResourceSelection)
	assert.Empty(t, executor.calls)
	assert.Len(t, planner.calls, 1)
}

func TestPendingResourceSelectionStillAcceptsOrdinalWithoutPlanner(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{Plan: accountBillingUnsupportedPlan()}}}
	executor := &mockExecutorFn{
		fn: func(action string, args map[string]any) (map[string]any, error) {
			switch action {
			case "GetCompShareInstanceMonitor":
				return map[string]any{"CPU": float64(12.5)}, nil
			default:
				return nil, fmt.Errorf("unexpected action %s", action)
			}
		},
	}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.InitWithContext("test user")
	candidates := []entity.InstanceSnapshot{
		testInstance("uhost-select-001", "select-a", "Running"),
		testInstance("uhost-select-002", "select-b", "Running"),
	}
	eng.pendingResourceSelection = &pendingResourceSelection{
		originalUserMsg: "CPU 高怎么办",
		plan:            phase1MonitorPlanWithoutTarget(),
		snapshot:        testSnapshotWithInstances(candidates...),
		candidates:      candidates,
		createdTurn:     eng.userTurn,
	}
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		EnabledIntents: []intent.Intent{intent.IntentBillingAccountUnsupported},
		Model:          "deepseek-v4-flash",
	})

	reply, err := eng.Chat(context.Background(), "2", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "CPU")
	assert.Equal(t, []string{"GetCompShareInstanceMonitor"}, executor.calls)
	assert.Len(t, planner.calls, 0, "ordinal selection should not spend planner call or be misread as a new request")
	assert.Nil(t, eng.pendingResourceSelection)
}

func TestBillingAccountUnsupportedDispatchOnlyMatchesThatIntent(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	for _, tc := range []struct {
		name   string
		intent intent.Intent
	}{
		{name: "pricing query", intent: intent.IntentPricingQuery},
		{name: "instance billing", intent: intent.IntentBillingInstance},
		{name: "refund estimate", intent: intent.IntentRefundEstimate},
		{name: "knowledge qa", intent: intent.IntentKnowledgeQA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply, handled := eng.tryBillingAccountUnsupportedDispatch(routerDispatchResult{
				result: intent.IntentRouterResult{Plan: intent.IntentRoute{
					SchemaVersion: intent.SchemaVersion,
					Intent:        tc.intent,
					Retrieval:     intent.Retrieval{Enabled: false},
					Confidence:    0.9,
				}},
			})
			assert.False(t, handled)
			assert.Empty(t, reply)
		})
	}
}

func TestBuildIntentPlannerMapsIncludesBillingAccountUnsupported(t *testing.T) {
	enabled, routes := BuildIntentPlannerMaps([]intent.Intent{intent.IntentBillingAccountUnsupported})
	_, ok := enabled[intent.IntentBillingAccountUnsupported]
	assert.True(t, ok, "billing_account_unsupported must be planner-dispatchable so it can refuse before tools/ReAct")
	_, ok = routes[intent.IntentBillingAccountUnsupported]
	assert.False(t, ok, "billing_account_unsupported is a direct refusal, not a route-handler intent")
}

func accountBillingUnsupportedPlan() intent.IntentRoute {
	return intent.IntentRoute{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentBillingAccountUnsupported,
		Slots: intent.Slots{
			TargetRefs: []intent.TargetRef{},
			Metrics:    []intent.Metric{},
			TimeWindow: nil,
		},
		RequiredTools: []string{},
		Retrieval:     intent.Retrieval{Enabled: false},
		HardBlockHint: true,
		Confidence:    0.9,
	}
}
