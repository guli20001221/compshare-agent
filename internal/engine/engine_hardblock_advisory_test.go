package engine

// Tests pinning the PR #61 design invariant: planner.HardBlockHint is
// advisory only — it ships to PlannerTrace.HardBlockHint for observability
// but does NOT participate in cutover routing.
//
// Sibling tests already verify the actually-executed hard-block sources
// produce the correct EngineHardBlockTrace.TriggeredBy value:
//   - keyword preblock        → TestResourceShortageHardBlock_NotifiesObserverWithoutStepEvent
//   - planner_intent dispatch → TestPlannerMonitorHistoryHardBlock_FiresObserverEvenWhenKeywordMisses
//   - post_llm cited contract → TestStage2BRetrievalCommonPredicateFallbacksDoNotCallRetriever
//
// The two tests here cover the inverse: HardBlockHint=true is silent unless
// another stage independently refuses. No "both" attribution is possible
// because the short-circuited stages are unobservable (see memory
// feedback_attribution_observable_only).

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCommonPlannerCandidateStatus_HardBlockHintAdvisoryOnly pins the core
// behavior change at unit level: HardBlockHint=true alone must produce
// CutoverStatusDispatched, not CutoverStatusFallbackHardBlockHint. The
// previous routing branch is gone — HardBlockHint is now observability
// only.
func TestCommonPlannerCandidateStatus_HardBlockHintAdvisoryOnly(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	cases := []struct {
		name       string
		result     intent.PlannerResult
		wantStatus intent.CutoverStatus
		wantOK     bool
	}{
		{
			name: "hint_true_high_confidence_dispatches",
			result: intent.PlannerResult{Plan: intent.Plan{
				SchemaVersion: intent.SchemaVersion,
				Intent:        intent.IntentBillingAccountUnsupported,
				HardBlockHint: true,
				Retrieval:     intent.Retrieval{Enabled: false},
				Confidence:    0.9,
			}},
			wantStatus: intent.CutoverStatusDispatched,
			wantOK:     true,
		},
		{
			name: "hint_false_high_confidence_dispatches",
			result: intent.PlannerResult{Plan: intent.Plan{
				SchemaVersion: intent.SchemaVersion,
				Intent:        intent.IntentBillingAccountUnsupported,
				HardBlockHint: false,
				Retrieval:     intent.Retrieval{Enabled: false},
				Confidence:    0.9,
			}},
			wantStatus: intent.CutoverStatusDispatched,
			wantOK:     true,
		},
		{
			name: "hint_true_low_confidence_still_falls_back",
			result: intent.PlannerResult{Plan: intent.Plan{
				SchemaVersion: intent.SchemaVersion,
				Intent:        intent.IntentBillingAccountUnsupported,
				HardBlockHint: true,
				Retrieval:     intent.Retrieval{Enabled: false},
				Confidence:    0.3,
			}},
			// HardBlockHint does NOT participate, but low confidence still
			// triggers fallback (the legitimate non-HardBlockHint reason).
			wantStatus: intent.CutoverStatusFallbackLowConfidence,
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
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{Plan: plan}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "normal answer"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	var hardBlocks []observability.EngineHardBlockTrace
	eng.SetHardBlockObserver(func(trace observability.EngineHardBlockTrace) {
		hardBlocks = append(hardBlocks, trace)
	})
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})

	// Neutral question — no resource_shortage / account_billing / monitor_history
	// keyword matches; planner classifies it as knowledge_qa with HardBlockHint
	// erroneously set. Pre-PR #61: this could refuse via ReAct guard drift.
	// Post-PR #61: must produce the LLM's normal answer.
	reply, err := eng.Chat(context.Background(), "如何创建一个 GPU 实例", noopStep)
	require.NoError(t, err)

	assert.Equal(t, "normal answer", reply,
		"HardBlockHint=true without keyword match must not refuse — user behavior must be stable")
	assert.Empty(t, hardBlocks,
		"advisory HardBlockHint must not synthesize an engine_hard_block on its own")
}
