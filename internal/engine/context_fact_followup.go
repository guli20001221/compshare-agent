package engine

import (
	"context"
	"strings"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/intent"
)

func (e *Engine) tryRecentFactFollowup(ctx context.Context, dispatch routerDispatchResult, userMsg string, onStep func(StepEvent)) (string, bool) {
	if e == nil || !e.sessionFactContextEnabled || !e.sessionStateHydrated || len(e.sessionState.RecentFacts) == 0 {
		return "", false
	}
	decision, err := e.resolveContextDecision(ctx, userMsg, dispatch.result.Plan.Intent, e.sessionState.ContextFrame)
	if err != nil || decision == nil || decision.Decision != ContextDecisionAnswerFollowup {
		return "", false
	}
	if reply, ok := e.tryRecentFactBillingFollowup(ctx, dispatch, *decision, onStep); ok {
		return reply, true
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
			Slots:         slotsForFactFollowupDecision(*decision),
			Confidence:    0.85,
		},
	}
	followupText := factFollowupUserText(userMsg, *decision)
	return e.tryRouteDispatch(ctx, resumed, followupText, onStep)
}

func (e *Engine) tryRecentFactBillingFollowup(ctx context.Context, dispatch routerDispatchResult, decision ContextDecision, onStep func(StepEvent)) (string, bool) {
	if decision.Target != ContextDecisionTargetBilling || contextDecisionBillingTopicIsRefund(decision.BillingTopic) {
		return "", false
	}
	selectedID := strings.TrimSpace(e.sessionState.SelectedInstanceID)
	if selectedID == "" {
		return "", false
	}
	inst := entity.InstanceSnapshot{
		UHostId: selectedID,
		Name:    strings.TrimSpace(e.sessionState.SelectedInstanceName),
	}
	if snapshot, ok, err := e.freshResourceSelectionSnapshot(ctx, dispatch.snapshot); err == nil && ok {
		if found, res := snapshot.ResolveByID(selectedID); res.Status == entity.ResolveHit && found != nil {
			inst = *found
		}
	}
	raw, failureClass := e.executeDiagnosisWithOutcome(ctx, "DiagnoseBilling", map[string]any{"UHostId": selectedID}, onStep)
	if failureClass == intent.HandlerFailureGenericRead {
		e.emitPlannerTrace(intent.IntentRouterResult{Plan: intent.IntentRoute{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentBillingInstance,
			Confidence:    0.85,
		}}, intent.RouteStatusFailureAfterTool, dispatch.latency)
		e.routeReadFailureThisTurn = true
		return "", false
	}
	reply := renderDiagnosisContinuationReply(inst, "DiagnoseBilling", raw)
	e.emitPlannerTrace(intent.IntentRouterResult{Plan: intent.IntentRoute{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentBillingInstance,
		Confidence:    0.85,
	}}, intent.RouteStatusDispatched, dispatch.latency)
	source := strings.TrimSpace(e.sessionState.SelectedInstanceSource)
	if source == "" {
		source = SelectedInstanceSourceObserved
	}
	e.recordSelectedInstanceIDWithSource(inst.UHostId, inst.Name, source)
	e.recordLastIntentFromPlan(intent.IntentRoute{Intent: intent.IntentBillingInstance})
	e.messages = append(e.messages, assistantMessage(reply))
	return reply, true
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

func slotsForFactFollowupDecision(decision ContextDecision) intent.Slots {
	slots := intent.Slots{}
	if gpu := strings.TrimSpace(decision.SlotUpdates["gpu_type"]); gpu != "" {
		slots.SearchQuery = gpu
	}
	if zone := strings.TrimSpace(decision.SlotUpdates["zone"]); zone != "" {
		slots.Zone = zone
	}
	return slots
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
