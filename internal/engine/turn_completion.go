package engine

import (
	"strings"

	"github.com/compshare-agent/internal/llm"
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
	e.turnModelProviderThisTurn = ""
	e.turnModelIDsThisTurn = nil
	e.turnModelAttemptsThisTurn = nil
	e.turnCompletionClassHint = ""
	e.turnCompletionReasonHint = ""
	e.runtimeFinishReasonThisTurn = ""
	e.turnCompletionEmittedThisTurn = false
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

	attempts := append([]observability.ModelAttemptTrace(nil), e.turnModelAttemptsThisTurn...)
	finishReasons := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		if attempt.FinishReason != "" {
			finishReasons = append(finishReasons, attempt.FinishReason)
		}
	}
	trace := observability.TurnCompletionTrace{
		RuntimeFinishReason:   string(e.runtimeFinishReasonThisTurn),
		ModelCalls:            e.turnModelCallsThisTurn,
		ModelProvider:         e.turnModelProviderThisTurn,
		ModelIDs:              append([]string(nil), e.turnModelIDsThisTurn...),
		ProviderFinishReasons: finishReasons,
		ModelAttempts:         attempts,
		ToolNames:             centralAgentToolNames(e.mutatingToolsEnabled, e.mutatingToolsEnabled && e.instanceOps != nil),
	}

	trace.Class, trace.Reason = e.classifyTurnCompletion()
	if e.turnCompletionObserver != nil {
		e.turnCompletionObserver(trace)
	}
}

// recordTurnModel receives only the client-boundary metadata, never a prompt
// or response. The engine uses one configured client today; retaining unique
// ids still makes an accidental future fallback visible instead of silently
// attributing a mixed turn to whichever model happened to run first.
func (e *Engine) recordTurnModel(call llm.OutboundCall) {
	if e == nil {
		return
	}
	provider := strings.TrimSpace(call.Provider)
	if provider != "" {
		if e.turnModelProviderThisTurn == "" {
			e.turnModelProviderThisTurn = provider
		} else if e.turnModelProviderThisTurn != provider {
			e.turnModelProviderThisTurn = "mixed"
		}
	}
	model := strings.TrimSpace(call.Model)
	if model == "" {
		return
	}
	for _, existing := range e.turnModelIDsThisTurn {
		if existing == model {
			return
		}
	}
	e.turnModelIDsThisTurn = append(e.turnModelIDsThisTurn, model)
}

func (e *Engine) recordTurnModelAttempt(result llm.OutboundCallResult) {
	if e == nil {
		return
	}
	e.turnModelAttemptsThisTurn = append(e.turnModelAttemptsThisTurn, observability.ModelAttemptTrace{
		AttemptInCall: result.AttemptInCall, LatencyMS: result.LatencyMS, Outcome: result.Outcome,
		ErrorClass: result.ErrorClass, Retried: result.Retried,
		FinishReason: result.StopReason, FirstChunkMS: result.ProviderFirstChunkMS,
		PromptTokens: result.PromptTokens, CachedPromptTokens: result.CachedPromptTokens,
		ToolCount: result.ToolCount, ToolWindowRunes: result.ToolWindowRunes, ToolWindowHash: result.ToolWindowHash,
	})
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
