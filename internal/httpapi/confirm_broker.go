package httpapi

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/compshare-agent/internal/store"
	"github.com/google/uuid"
)

var (
	ErrConfirmationNotFound = errors.New("confirmation not found or already resolved")
	ErrConfirmationOwner    = errors.New("confirmation does not belong to this session/owner")
)

type pendingConfirm struct {
	sessionID string
	owner     store.Owner
	ch        chan bool
}

// ConfirmBroker mediates between an SSE Chat handler (which blocks waiting
// for user confirmation) and the ConfirmCSAgentAction handler (which delivers
// the user's confirm/deny decision). Each pending confirmation is identified
// by a unique UUID so stale confirms from slow clients cannot accidentally
// resolve a newer pending.
type ConfirmBroker struct {
	mu      sync.Mutex
	pending map[string]*pendingConfirm
}

func NewConfirmBroker() *ConfirmBroker {
	return &ConfirmBroker{pending: make(map[string]*pendingConfirm)}
}

// Register creates a new pending confirmation and returns its unique ID plus
// a receive-only channel. The caller blocks on the channel; the result is
// true (confirmed) or false (denied/timeout/cancelled).
func (b *ConfirmBroker) Register(sessionID string, owner store.Owner) (string, <-chan bool) {
	id := uuid.NewString()
	ch := make(chan bool, 1)
	b.mu.Lock()
	b.pending[id] = &pendingConfirm{sessionID: sessionID, owner: owner, ch: ch}
	b.mu.Unlock()
	return id, ch
}

// Resolve delivers the user's decision. Returns ErrConfirmationNotFound if
// the ID is unknown, or ErrConfirmationOwner if the sessionID/owner doesn't
// match — preventing cross-session confirmation hijacking.
func (b *ConfirmBroker) Resolve(confirmationID, sessionID string, owner store.Owner, confirmed bool) error {
	b.mu.Lock()
	p, ok := b.pending[confirmationID]
	if !ok {
		b.mu.Unlock()
		return ErrConfirmationNotFound
	}
	if p.sessionID != sessionID || p.owner != owner {
		b.mu.Unlock()
		return ErrConfirmationOwner
	}
	delete(b.pending, confirmationID)
	b.mu.Unlock()
	p.ch <- confirmed
	close(p.ch)
	return nil
}

// Cancel removes a pending confirmation without sending a value. Called when
// the SSE connection drops before the user responds.
func (b *ConfirmBroker) Cancel(confirmationID string) {
	b.mu.Lock()
	p, ok := b.pending[confirmationID]
	if ok {
		delete(b.pending, confirmationID)
	}
	b.mu.Unlock()
	if ok {
		close(p.ch)
	}
}

// WaitForConfirmation blocks until the confirmation is resolved, the context
// is cancelled (SSE disconnect), or the timeout expires. Returns true only
// if the user explicitly confirmed.
func WaitForConfirmation(ctx context.Context, ch <-chan bool, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case confirmed, ok := <-ch:
		return ok && confirmed
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}
