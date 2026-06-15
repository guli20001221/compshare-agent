package observability

import (
	"context"
	"errors"
)

// outcome.go derives the per-turn outcome-attribution axes at Finish, with zero
// judge calls — the same derive-at-Finish discipline as DeriveActualExecutionTier
// (trace.go). It closes the "~25% of turns have no attribution" dark hole
// (clusters #1/#3/#5 in the trace observability spec) by recording WHY a turn
// terminated and HOW it ended, on four orthogonal axes.
//
// The derives are methods on TraceRecord so they can read record-internal signals
// (EngineHardBlock, RateLimit, Retrieval) and combine them with the external
// FinishSignals the engine/caller knows. Each derive is pure and individually
// unit-tested for forced precedence (outcome_test.go) so the four axes cannot
// self-mislabel.

// TerminatedBy* are the values for OutcomeTrace.TerminatedBy — the single coarse
// "why did this turn stop" axis. Derived with first-match-wins precedence
// (see DeriveTerminatedBy): blocked > user_cancel > timeout > budget > error >
// empty_reply > done.
const (
	TerminatedByDone       = "done"
	TerminatedByUserCancel = "user_cancel"
	TerminatedByEmptyReply = "empty_reply"
	TerminatedByBudget     = "budget"
	TerminatedByError      = "error"
	TerminatedByTimeout    = "timeout"
	TerminatedByBlocked    = "blocked"
)

// AbortCause* refine TerminatedBy ∈ {user_cancel, empty_reply}. The confirm-flow
// causes (confirm_timeout / confirm_declined) land in Phase 1b via the confirm
// broker hook — they are not derivable from the turn record alone.
const (
	AbortCauseClientDisconnect = "client_disconnect"
	AbortCauseLLMEmptyStream   = "llm_empty_stream"
)

// Resolution* are the DETERMINISTIC subset of outcome.resolution the engine fills.
// The resolved-vs-partial distinction for a delivered answer is LEFT EMPTY for the
// online judge — it must never be defaulted to "resolved".
const (
	ResolutionBlocked = "blocked"
	ResolutionRefused = "refused"
)

// FinishSignals carries the per-turn terminal facts the trace record cannot
// observe on its own: the chat error, whether the final reply was empty, and the
// ReAct-loop round count / ceiling state. Production callers populate it (via the
// recorders' SetTerminalSignals); a zero value derives to TerminatedByDone for a
// clean turn, which is the correct default for unit fixtures that do not set it.
type FinishSignals struct {
	// ChatErr is the error returned by Engine.Chat (nil on success). Drives the
	// user_cancel / timeout / error terminuses via errors.Is.
	ChatErr error
	// ReplyEmpty is true when the turn ended with chatErr==nil and an empty reply
	// (the "dark-hole-within-the-dark-hole": empty LLM streams that previously hid
	// inside status="success").
	ReplyEmpty bool
	// ReactRounds is the number of ReAct loop rounds entered this turn (0 when the
	// turn never ran the loop — routing / RAG / pre-block paths).
	ReactRounds int
	// RoundCeilingHit is true when the ReAct loop exhausted maxReActRounds without
	// producing a final answer (engine.go round-ceiling path), which returns a
	// non-empty canned reply and no hard-block — so the budget terminus is
	// otherwise underivable from the record.
	RoundCeilingHit bool
}

// FinalizeOutcome stamps the four outcome-attribution axes (plus react_rounds /
// budget_hit) onto the record. Call it once at Finish, after every other signal
// is final and before the record is handed to the sink.
func (r *TraceRecord) FinalizeOutcome(s FinishSignals) {
	tb := r.DeriveTerminatedBy(s)
	r.Outcome.TerminatedBy = tb
	r.Outcome.AbortCause = r.DeriveAbortCause(s, tb)
	r.Outcome.ErrorClass = r.DeriveErrorClass(s)
	r.Outcome.Resolution = r.DeriveResolution(s, tb)
	if s.ReactRounds > 0 {
		r.Outcome.ReactRounds = s.ReactRounds
	}
	r.Outcome.BudgetHit = r.budgetExhausted(s)
}

// DeriveTerminatedBy maps the turn to its coarse termination cause, first match
// wins (the spec precedence): blocked > user_cancel > timeout > budget > error >
// empty_reply > done.
func (r TraceRecord) DeriveTerminatedBy(s FinishSignals) string {
	if r.isGenuineBlock() {
		return TerminatedByBlocked
	}
	if errors.Is(s.ChatErr, context.Canceled) {
		return TerminatedByUserCancel
	}
	if errors.Is(s.ChatErr, context.DeadlineExceeded) {
		return TerminatedByTimeout
	}
	if r.budgetExhausted(s) {
		return TerminatedByBudget
	}
	if s.ChatErr != nil {
		return TerminatedByError
	}
	if s.ReplyEmpty {
		return TerminatedByEmptyReply
	}
	return TerminatedByDone
}

// DeriveAbortCause refines the two terminuses that have an observable sub-cause.
func (r TraceRecord) DeriveAbortCause(s FinishSignals, terminatedBy string) string {
	switch terminatedBy {
	case TerminatedByUserCancel:
		return AbortCauseClientDisconnect
	case TerminatedByEmptyReply:
		return AbortCauseLLMEmptyStream
	}
	return ""
}

// DeriveErrorClass classifies the chat error for the error_class axis (shared
// classifier; "" when chatErr is nil).
func (r TraceRecord) DeriveErrorClass(s FinishSignals) string {
	return ClassifyErrorClass(s.ChatErr)
}

// DeriveResolution fills only the deterministic subset: a blocked turn resolved to
// "blocked", a no-evidence RAG refusal to "refused". A delivered answer is left
// "" — resolved-vs-partial is the online judge's call, never defaulted here.
func (r TraceRecord) DeriveResolution(s FinishSignals, terminatedBy string) string {
	if terminatedBy == TerminatedByBlocked {
		return ResolutionBlocked
	}
	if r.Retrieval.RefusedReason != "" {
		return ResolutionRefused
	}
	return ""
}

// isGenuineBlock reports a real engine hard-block or rate-limit denial. It
// EXCLUDES two hard-block categories that are not "blocked" terminuses:
//   - the synthetic "chat_error" marker the HTTP recorder stamps when chatErr!=nil
//     (that turn is an error terminus), and
//   - token-budget exhaustion (that is a budget terminus),
//
// so the precedence chain reaches the error / budget branches for those turns.
func (r TraceRecord) isGenuineBlock() bool {
	if r.RateLimit.Checked && !r.RateLimit.Allowed {
		return true
	}
	if r.EngineHardBlock.Hit {
		switch r.EngineHardBlock.Category {
		case HardBlockCategoryChatError, HardBlockCategoryTokenBudget:
			return false
		}
		return true
	}
	return false
}

// budgetExhausted reports a per-turn budget terminus: either the token budget
// (observable in the record as the token-budget hard-block category) or the ReAct
// round ceiling (a FinishSignal, since that path emits no hard-block).
func (r TraceRecord) budgetExhausted(s FinishSignals) bool {
	if r.EngineHardBlock.Hit && r.EngineHardBlock.Category == HardBlockCategoryTokenBudget {
		return true
	}
	return s.RoundCeilingHit
}
