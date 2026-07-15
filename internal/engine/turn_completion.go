package engine

import (
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/tools"
)

// SetTurnCompletionObserver wires the one-per-turn final control-flow record.
func (e *Engine) SetTurnCompletionObserver(observer func(observability.TurnCompletionTrace)) {
	if e == nil {
		return
	}
	e.turnCompletionObserver = observer
}

func (e *Engine) resetTurnCompletion() {
	if e == nil {
		return
	}
	e.turnModelCallsThisTurn = 0
	e.turnCompletionClassHint = ""
	e.turnCompletionReasonHint = ""
	e.turnCompletionEmittedThisTurn = false
	e.lastPlannerRouteStatusThisTurn = intent.RouteStatusNone
	e.lastPlannerIntentForCompletionThisTurn = ""
	e.hardBlockTraceThisTurn = observability.EngineHardBlockTrace{}
}

// markTurnCompletion records a terminal path that cannot be reconstructed from
// planner status or ReAct rounds. The first terminal marker wins; a standing
// hard block is handled separately and always has higher priority.
func (e *Engine) markTurnCompletion(class, reason string) {
	if e == nil || e.turnCompletionClassHint != "" {
		return
	}
	e.turnCompletionClassHint = class
	e.turnCompletionReasonHint = reason
}

// emitTurnCompletion is called by the single top-level defer in
// ChatWithOptions. The guard makes duplicate emission impossible even if a
// future refactor also invokes it explicitly.
func (e *Engine) emitTurnCompletion() {
	if e == nil || e.turnCompletionEmittedThisTurn {
		return
	}
	e.turnCompletionEmittedThisTurn = true

	trace := observability.TurnCompletionTrace{
		ModelCalls:      e.turnModelCallsThisTurn,
		ContextDecision: observability.CompletionDecisionNotInvoked,
		ToolScope:       string(tools.ToolScopeNamed),
	}
	if e.contextDecisionTraceSeenThisTurn {
		ctxTrace := e.contextDecisionTraceThisTurn
		trace.ContextDecision = ctxTrace.Decision
		if trace.ContextDecision == "" {
			if ctxTrace.Error != "" {
				trace.ContextDecision = observability.CompletionDecisionError
			} else {
				trace.ContextDecision = observability.CompletionDecisionNotInvoked
			}
		}
		trace.ReadSet = append([]string(nil), ctxTrace.ReadSet...)
		trace.StateDelta = append([]string(nil), ctxTrace.StateDelta...)
		trace.ToolScope = ctxTrace.ToolScope
		trace.ToolNames = append([]string(nil), ctxTrace.ToolNames...)
	} else if e.reactRoundsThisTurn > 0 || e.lastPlannerRouteStatusThisTurn != intent.RouteStatusNone {
		route := e.lastPlannerIntentForCompletionThisTurn
		if route == "" {
			route = e.lastPlannerIntentThisTurn
		}
		scope := toolScopeForIntent(route)
		trace.ToolScope = string(scope.Mode)
		trace.ToolNames = append([]string(nil), scope.Names...)
	}

	trace.Class, trace.Reason = e.classifyTurnCompletion()
	if e.turnCompletionObserver != nil {
		e.turnCompletionObserver(trace)
	}
}

func (e *Engine) classifyTurnCompletion() (string, string) {
	if e.hardBlockStandingThisTurn {
		if e.hardBlockTraceThisTurn.Category == observability.HardBlockCategoryTokenBudget {
			return observability.CompletionClassSafetyBlock, observability.CompletionReasonTokenBudget
		}
		return observability.CompletionClassSafetyBlock, observability.CompletionReasonPolicyBlock
	}
	if e.turnCompletionClassHint != "" {
		return e.turnCompletionClassHint, e.turnCompletionReasonHint
	}
	if e.contextDecisionTraceSeenThisTurn && e.contextDecisionTraceThisTurn.Decision == ContextDecisionClarify {
		return observability.CompletionClassStructuredClarify, observability.CompletionReasonContextClarification
	}
	if e.reactRoundsThisTurn > 0 {
		return observability.CompletionClassAgent, observability.CompletionReasonAgentLoop
	}

	switch e.lastPlannerRouteStatusThisTurn {
	case intent.RouteStatusDispatchedAgent, intent.RouteStatusDispatchedKnowledgeAgentLoop:
		return observability.CompletionClassAgent, observability.CompletionReasonAgentDispatch
	case intent.RouteStatusDispatchedRetrieval:
		return observability.CompletionClassAgent, observability.CompletionReasonModelAssistedAnswer
	case intent.RouteStatusDispatched:
		return observability.CompletionClassDeterministicAnswer, observability.CompletionReasonDirectDispatch
	case intent.RouteStatusSelectionRequired, intent.RouteStatusFallbackUnresolvedTarget:
		return observability.CompletionClassStructuredClarify, observability.CompletionReasonSelectionRequired
	case intent.RouteStatusFallbackTimeWindow:
		return observability.CompletionClassStructuredClarify, observability.CompletionReasonMissingTimeWindow
	case intent.RouteStatusFailureAfterTool:
		return observability.CompletionClassDeterministicAnswer, observability.CompletionReasonHandlerFailure
	case intent.RouteStatusFallbackRetrievalMiss:
		return observability.CompletionClassDeterministicAnswer, observability.CompletionReasonRetrievalNoEvidence
	case intent.RouteStatusFallbackRetrievalDisabled:
		return observability.CompletionClassDeterministicAnswer, observability.CompletionReasonRetrievalUnavailable
	case intent.RouteStatusFallbackInvalid:
		return observability.CompletionClassParserFailureFallback, observability.CompletionReasonRouteParseFailure
	case intent.RouteStatusFallbackLowConfidence, intent.RouteStatusFallbackIneligible:
		return observability.CompletionClassParserFailureFallback, observability.CompletionReasonRouteFallbackWithoutModel
	}
	if e.turnModelCallsThisTurn > 0 {
		return observability.CompletionClassAgent, observability.CompletionReasonModelAssistedAnswer
	}
	return observability.CompletionClassParserFailureFallback, observability.CompletionReasonUnclassifiedZeroModelExit
}
