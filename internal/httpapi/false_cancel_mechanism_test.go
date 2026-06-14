package httpapi

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestRepro_WaitForConfirmation_TimeoutAndDisconnectAreIndistinguishableFromDecline
// pins the root mechanism behind the production false-cancel cluster.
//
// WaitForConfirmation collapses THREE distinct outcomes into the same zero
// ConfirmDecision{Confirmed:false}:
//   - explicit user decline  (a value sent on the channel)
//   - 60s timeout            (no one resolved the confirmation)
//   - client disconnect      (ctx cancelled)
//   - closed channel
//
// Because the caller only sees `.Confirmed == false`, the engine narrates all
// of them as "用户取消了操作" → "好的，已取消X操作。". On a non-interactive
// channel (企微) the card is never resolved, so EVERY mutating op times out and
// self-cancels with that false claim.
//
// This test documents the current (buggy) equivalence. The fix must let the
// caller tell "the user said no" apart from "no one answered".
func TestRepro_WaitForConfirmation_TimeoutAndDisconnectAreIndistinguishableFromDecline(t *testing.T) {
	// 1. Explicit decline: a real {Confirmed:false} delivered on the channel.
	declineCh := make(chan ConfirmDecision, 1)
	declineCh <- ConfirmDecision{Confirmed: false}
	decline := WaitForConfirmation(context.Background(), declineCh, time.Second)

	// 2. Timeout: nobody ever resolves within the window.
	timeoutCh := make(chan ConfirmDecision) // never written
	timeout := WaitForConfirmation(context.Background(), timeoutCh, 10*time.Millisecond)

	// 3. Client disconnect: context cancelled before resolution.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	disconnectCh := make(chan ConfirmDecision) // never written
	disconnect := WaitForConfirmation(ctx, disconnectCh, time.Second)

	// All three are byte-identical at the boolean the engine actually reads.
	assert.False(t, decline.Confirmed, "explicit decline is false")
	assert.False(t, timeout.Confirmed, "timeout is false")
	assert.False(t, disconnect.Confirmed, "disconnect is false")
	assert.Equal(t, decline, timeout,
		"BUG: timeout is indistinguishable from an explicit decline")
	assert.Equal(t, decline, disconnect,
		"BUG: disconnect is indistinguishable from an explicit decline")
}
