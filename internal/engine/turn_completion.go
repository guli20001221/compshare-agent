package engine

import (
	"github.com/compshare-agent/internal/observability"
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
	e.hardBlockTraceThisTurn = observability.EngineHardBlockTrace{}
	e.lastOutboundToolWindowScopeThisTurn = modelToolWindowScope{}
	e.lastOutboundToolNamesThisTurn = nil
	e.lastOutboundToolWindowObservedThisTurn = false
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
		ToolScopePhase:  "no_model_window",
	}
	if e.lastOutboundToolWindowObservedThisTurn {
		scope := e.lastOutboundToolWindowScopeThisTurn
		trace.ToolScope = string(scope.Mode)
		trace.ToolScopePhase = "last_outbound_agent_tool_window"
		trace.ToolScopeReason = scope.Reason
		trace.ToolNames = append([]string(nil), e.lastOutboundToolNamesThisTurn...)
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
	if e.reactRoundsThisTurn > 0 {
		return observability.CompletionClassAgent, observability.CompletionReasonAgentLoop
	}

	if e.turnModelCallsThisTurn > 0 {
		return observability.CompletionClassAgent, observability.CompletionReasonModelAssistedAnswer
	}
	return observability.CompletionClassParserFailureFallback, observability.CompletionReasonUnclassifiedZeroModelExit
}
