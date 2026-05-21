package server

import (
	"sync"
	"time"

	"github.com/compshare-agent/internal/engine"
)

// confirmBridge couples the engine's synchronous ConfirmFunc API to the
// asynchronous WS round-trip. Engine.Chat invokes confirmFn on a tool-call
// gate; we transform that into a ServerMsgConfirmRequired frame, then
// block (with timeout) until the client replies with a confirm_response
// frame whose RequestUUID matches.
//
// Lifecycle:
//   - One bridge per WS connection (created in handleWS).
//   - .ConfirmFn(sendChan) returns the engine.ConfirmFunc the session
//     should use. The closure captures sendChan so each invocation can
//     enqueue the confirm_required frame.
//   - .OnClientResponse(requestUUID, confirmed) is called from the reader
//     loop on each ClientMsgConfirmResponse frame. It routes the answer
//     to the pending waiter keyed by requestUUID.
type confirmBridge struct {
	timeout time.Duration

	mu      sync.Mutex
	pending map[string]chan bool
}

const defaultConfirmTimeout = 30 * time.Second

func newConfirmBridge() *confirmBridge {
	return &confirmBridge{
		timeout: defaultConfirmTimeout,
		pending: make(map[string]chan bool),
	}
}

// ConfirmFn returns an engine.ConfirmFunc that pushes a confirm_required
// frame onto sendChan and waits for the client's reply. The current
// confirm request uses requestUUID = uuid generated per call; the WS
// handler scopes this to the active turn before invoking.
//
// On timeout we return false — the safest default. The Engine treats
// false as "user denied", which translates to a polite refusal to the
// client rather than executing a mutating action silently.
//
// SECURITY: the closure does not capture or expose the TenantCtx; the
// confirm frame contains only the action name + args. Tenant context
// stays in the WS connection state.
func (b *confirmBridge) ConfirmFn(sendChan chan<- ServerMessage, currentRequestUUID func() string) engine.ConfirmFunc {
	return func(action string, args map[string]any) bool {
		uuid := currentRequestUUID()
		ch := make(chan bool, 1)
		b.mu.Lock()
		b.pending[uuid] = ch
		b.mu.Unlock()
		defer func() {
			b.mu.Lock()
			delete(b.pending, uuid)
			b.mu.Unlock()
		}()

		select {
		case sendChan <- ServerMessage{
			Type:        ServerMsgConfirmRequired,
			RequestUUID: uuid,
			Action:      action,
			Args:        args,
		}:
		case <-time.After(b.timeout):
			// sendChan saturated for >timeout → client is broken; deny.
			return false
		}

		select {
		case confirmed := <-ch:
			return confirmed
		case <-time.After(b.timeout):
			return false
		}
	}
}

// OnClientResponse routes a client confirm_response to the waiter. Returns
// true if a waiter was found (and unblocked). The reader loop logs but
// otherwise ignores responses that do not match any pending request — the
// client likely sent a confirm after a timeout/cancellation.
func (b *confirmBridge) OnClientResponse(requestUUID string, confirmed bool) bool {
	b.mu.Lock()
	ch, ok := b.pending[requestUUID]
	b.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- confirmed:
		return true
	default:
		// Buffered chan size 1; if it's already full something is racing.
		return false
	}
}
