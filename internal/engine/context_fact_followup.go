package engine

import (
	"context"
	"strings"

	"github.com/compshare-agent/internal/intent"
)

func (e *Engine) tryRecentFactFollowup(ctx context.Context, dispatch routerDispatchResult, userMsg string, onStep func(StepEvent)) (string, bool) {
	if e == nil || !ContextContinuationEnabled() || !e.sessionFactContextEnabled || !e.sessionStateHydrated || len(e.sessionState.RecentFacts) == 0 {
		return "", false
	}
	decision, err := e.resolveContextDecision(ctx, userMsg, dispatch.result.Plan.Intent, e.sessionState.ContextFrame)
	if err != nil || decision == nil || decision.Decision != ContextDecisionAnswerFollowup {
		return "", false
	}
	targetIntent, ok := intentForFactFollowupDecision(*decision)
	if !ok {
		return "", false
	}
	resumed := dispatch
	resumed.result = intent.IntentRouterResult{
		Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        targetIntent,
			Confidence:    0.85,
		},
	}
	followupText := factFollowupUserText(userMsg, *decision)
	return e.tryRouteDispatch(ctx, resumed, followupText, onStep)
}

func intentForFactFollowupDecision(decision ContextDecision) (intent.Intent, bool) {
	switch decision.Target {
	case ContextDecisionTargetStock:
		return intent.IntentStockAvailability, true
	case ContextDecisionTargetPricing:
		if contextDecisionBillingTopicIsRefund(decision.BillingTopic) {
			return intent.IntentRefundEstimate, true
		}
		return intent.IntentPricingQuery, true
	case ContextDecisionTargetBilling:
		if contextDecisionBillingTopicIsRefund(decision.BillingTopic) {
			return intent.IntentRefundEstimate, true
		}
		return "", false
	default:
		return "", false
	}
}

func contextDecisionBillingTopicIsRefund(topic string) bool {
	switch strings.ToLower(strings.TrimSpace(topic)) {
	case "refund", "refund_estimate":
		return true
	default:
		return false
	}
}

func factFollowupUserText(userMsg string, decision ContextDecision) string {
	parts := []string{strings.TrimSpace(userMsg)}
	for _, key := range []string{"gpu_type", "zone"} {
		if value := strings.TrimSpace(decision.SlotUpdates[key]); value != "" && !strings.Contains(parts[0], value) {
			parts = append(parts, value)
		}
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}
