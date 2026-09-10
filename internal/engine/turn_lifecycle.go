package engine

import (
	"time"

	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/security"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"

	openai "github.com/sashabaranov/go-openai"
)

// The opening of a turn, split out of ChatWithOptions so the ReAct loop is what
// reading that function shows you. Everything here runs exactly once, before the
// first model call, and nothing here decides an outcome.

// installTurnConfirmation overrides the session's confirmation callbacks with
// this turn's, wrapping each so every card's terminal state reaches the trace,
// and returns the restore its caller defers. The overrides are per-turn on
// session-scoped fields: a turn that supplies its own callbacks must not leave
// them installed for the next turn, and a turn that supplies none must still
// record the terminal state of the cards the session's own callback answers.
//
// Restores run in reverse installation order, matching the defer stack these
// three used to register individually.
func (e *Engine) installTurnConfirmation(opts ChatOptions) func() {
	var restores []func()

	if opts.ConfirmResultFunc != nil || opts.ConfirmFunc != nil || e.confirmFn != nil {
		origConfirm := e.confirmFn
		confirm := e.confirmFn
		if opts.ConfirmFunc != nil {
			confirm = ConfirmFunc(opts.ConfirmFunc)
		}
		wrappedConfirm := ConfirmFunc(func(action string, args map[string]any) bool {
			started := time.Now()
			result := ConfirmationResult{}
			if opts.ConfirmResultFunc != nil {
				result = opts.ConfirmResultFunc(action, args)
			} else if confirm != nil {
				result.Confirmed = confirm(action, args)
			}
			e.recordConfirmationResult(action, result, started, nil, nil)
			// Same value trace records, kept for the user-facing sentence. Set on
			// the approval path too, so a later refusal can never inherit an
			// earlier card's reason.
			e.lastConfirmationTerminalReason = observability.NormalizeConfirmationTerminalReason(result.Confirmed, result.TerminalReason)
			return result.Confirmed
		})
		e.confirmFn = wrappedConfirm
		e.safeExecutor.SetConfirmFunc(tools.ConfirmFunc(wrappedConfirm))
		restores = append(restores, func() {
			e.confirmFn = origConfirm
			e.safeExecutor.SetConfirmFunc(tools.ConfirmFunc(origConfirm))
		})
	}

	// Per-turn editable-form gate; HTTP wires it only for clients that advertise support.
	if opts.ConfirmEditsFunc != nil || e.confirmEditsFn != nil {
		origEdits := e.confirmEditsFn
		confirmEdits := e.confirmEditsFn
		if opts.ConfirmEditsFunc != nil {
			confirmEdits = opts.ConfirmEditsFunc
		}
		e.confirmEditsFn = func(action string, args map[string]any, form *workflow.ConfirmForm) workflow.ConfirmResolution {
			started := time.Now()
			resolution := confirmEdits(action, args, form)
			e.recordConfirmationResult(action, ConfirmationResult{
				Confirmed:      resolution.Confirmed,
				TerminalReason: resolution.TerminalReason,
			}, started, args, form)
			return resolution
		}
		restores = append(restores, func() { e.confirmEditsFn = origEdits })
	}

	if opts.GuidedCreate {
		origGuidedCreate := e.guidedCreate
		e.guidedCreate = true
		restores = append(restores, func() { e.guidedCreate = origGuidedCreate })
	}

	return func() {
		for i := len(restores) - 1; i >= 0; i-- {
			restores[i]()
		}
	}
}

// beginTurn puts the turn's opening state in place and appends the user's
// message. The carried instance binding is aged out and snapshotted before any
// mid-turn re-bind, the context card is compiled from that same instant, the
// system prompt is rebuilt from it, and history is trimmed before the append —
// so the raw-history budget is charged for what this turn inherited, not for the
// message it is about to add.
//
// Target meaning belongs to the Agent reading canonical conversation, not to a
// turn-entry name scan: nothing here reads userMsg for a target. Only confirmed
// workflows and actual tool observations update the persisted target context.
func (e *Engine) beginTurn(userMsg, llmCurrentUserMsg, turnID string, opts ChatOptions) {
	continuityNow := time.Now()
	e.expireStaleSelectedInstance(continuityNow)
	e.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(e, userMsg, turnID, continuityNow)
	e.turnContextViewReady = true
	e.selectedInstanceIDAtTurnStart = e.sessionState.SelectedInstanceID
	e.selectedInstanceSourceAtTurnStart = e.sessionState.SelectedInstanceSource
	e.selectedInstanceFreshnessAtTurnStart = normalizedSelectedInstanceFreshness(e.sessionState)
	// Reset the per-turn binding observables refreshSystemPrompt fills next.
	e.instanceResolutionSourceThisTurn = ""
	e.refreshSystemPrompt()

	e.trimHistory()

	// userMsg remains the original text for argument provenance; the appended
	// message carries image evidence into conversation history so the ReAct LLM
	// can reference it. The recognized text is fenced as untrusted reference data
	// (see WrapScreenshotContext) — the httpapi persist path MUST produce
	// byte-identical text because it is rehydrated and re-fed to the LLM on later
	// turns.
	llmUserMsg := llmCurrentUserMsg
	if opts.ImageContext != "" {
		// Screenshot OCR is fallible reference data and can never mint a private
		// credential capability. Remove any credential/PII before it reaches the
		// main model, matching the durable user-message boundary.
		llmUserMsg = WrapScreenshotContext(security.RedactUserConversationText(opts.ImageContext), llmCurrentUserMsg)
	}
	e.messages = append(e.messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: llmUserMsg,
	})
}
