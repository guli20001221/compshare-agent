package engine

import (
	"time"

	"github.com/compshare-agent/internal/observability"
)

// recordConfirmationResult is the single bridge from the confirmation harness
// into the trace. It is called after the transport has stopped waiting, so the
// elapsed time measures user-facing card latency rather than model time.
func (e *Engine) recordConfirmationResult(action string, result ConfirmationResult, started time.Time) {
	if e == nil {
		return
	}
	reason := observability.NormalizeConfirmationTerminalReason(result.Confirmed, result.TerminalReason)
	if e.confirmationTraceObserver != nil {
		e.confirmationTraceObserver(observability.ConfirmationTrace{
			Action:         action,
			State:          observability.ConfirmationStateForTerminalReason(reason),
			TerminalReason: reason,
			ElapsedMS:      time.Since(started).Milliseconds(),
		})
	}
}
