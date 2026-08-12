package httpapi

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/workflow"
	"github.com/google/uuid"
)

var (
	ErrConfirmationNotFound = errors.New("confirmation not found or already resolved")
	ErrConfirmationOwner    = errors.New("confirmation does not belong to this owner")
	// ErrOverridesNotAllowed is returned when a ConfirmCSAgentAction carries
	// Overrides but the pending confirmation never offered an editable form.
	ErrOverridesNotAllowed = errors.New("this confirmation does not accept overrides")
)

// ConfirmDecision is the user's resolution of a pending confirmation:
// confirm/deny plus (form confirmations only) validated field overrides.
type ConfirmDecision struct {
	Confirmed bool
	Overrides map[string]string
}

type pendingConfirm struct {
	sessionID string
	owner     store.Owner
	ch        chan ConfirmDecision
	// form, when non-nil, is the editable form emitted with this confirmation.
	// Overrides are accepted only against it (whitelist validation); nil
	// rejects any Overrides (legacy boolean confirmation).
	form *workflow.ConfirmForm
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
// a receive-only channel. The caller blocks on the channel; the decision
// carries confirmed/denied (Overrides always empty — no form was offered).
func (b *ConfirmBroker) Register(sessionID string, owner store.Owner) (string, <-chan ConfirmDecision) {
	return b.register(sessionID, owner, nil)
}

// RegisterWithForm is Register plus the editable form emitted with the
// confirmation; Resolve validates any Overrides against it.
func (b *ConfirmBroker) RegisterWithForm(sessionID string, owner store.Owner, form *workflow.ConfirmForm) (string, <-chan ConfirmDecision) {
	return b.register(sessionID, owner, form)
}

func (b *ConfirmBroker) register(sessionID string, owner store.Owner, form *workflow.ConfirmForm) (string, <-chan ConfirmDecision) {
	id := uuid.NewString()
	ch := make(chan ConfirmDecision, 1)
	b.mu.Lock()
	b.pending[id] = &pendingConfirm{sessionID: sessionID, owner: owner, ch: ch, form: form}
	b.mu.Unlock()
	return id, ch
}

// Resolve delivers the user's decision. Returns ErrConfirmationNotFound if
// the ID is unknown, or ErrConfirmationOwner if the OWNER (tenant) doesn't
// match — preventing cross-tenant confirmation hijacking.
//
// The confirmation is bound to its unguessable ConfirmationId + owner, NOT to
// the session label. Stale-session recovery (handlers_chat.go getOrCreateSession)
// can mint a new session id mid-turn, so the confirm frame's SessionId may
// legitimately differ from the one the confirmation was registered under.
// Enforcing session equality here false-rejected those confirms with
// ErrConfirmationOwner (the create-flow "[Forbidden] ... session/owner" bug).
// Only owner is enforced now: the random ConfirmationId already prevents any
// same-owner cross-session resolution, and the decision is delivered to the
// exact registering turn's channel, so a session-label drift is safe to
// resolve — it is only recorded (below) for observability.
//
// A CONFIRMED decision carrying Overrides is validated against the pending
// confirmation's form (whitelist: editable fields, offered option values
// only). On validation failure the pending entry is KEPT (the client may fix
// and resend; the waiter's timeout still applies) and the error is returned
// for an error frame. A deny ignores Overrides — denial needs no validation.
func (b *ConfirmBroker) Resolve(confirmationID, sessionID string, owner store.Owner, decision ConfirmDecision) error {
	deliver, err := b.ClaimResolution(confirmationID, sessionID, owner, decision)
	if err != nil {
		return err
	}
	deliver()
	return nil
}

// ClaimResolution is Resolve split in two: it performs the whole validated,
// locked claim — id lookup, owner check, override validation, removal from the
// pending map — and returns the func that actually hands the decision to the
// waiting turn. Nothing is delivered until the caller calls it.
//
// The split exists so a transport can ACKNOWLEDGE a resolution before waking the
// turn it resolves. Delivering first is a race the transport cannot win: the
// decision unblocks the chat goroutine, which may finish the turn, write `done`
// and cancel the connection context before this goroutine gets to write its
// acknowledgement — and the client would then be told its accepted (already
// executed) action had failed. Claiming is the part that must be atomic;
// delivery is the part that must come last.
//
// The returned deliver is single-use and safe to call exactly once: the entry is
// already out of the map, so no second Resolve can reach the same channel.
func (b *ConfirmBroker) ClaimResolution(confirmationID, sessionID string, owner store.Owner, decision ConfirmDecision) (func(), error) {
	b.mu.Lock()
	p, ok := b.pending[confirmationID]
	if !ok {
		b.mu.Unlock()
		return nil, ErrConfirmationNotFound
	}
	if p.owner != owner {
		b.mu.Unlock()
		return nil, ErrConfirmationOwner
	}
	if decision.Confirmed && len(decision.Overrides) > 0 {
		if p.form == nil {
			b.mu.Unlock()
			return nil, ErrOverridesNotAllowed
		}
		if err := p.form.ValidateOverrides(decision.Overrides); err != nil {
			b.mu.Unlock()
			return nil, err
		}
	}
	if !decision.Confirmed {
		decision.Overrides = nil
	}
	registeredSession := p.sessionID
	delete(b.pending, confirmationID)
	b.mu.Unlock()
	if registeredSession != sessionID {
		// Legitimate under stale-session recovery: the confirm arrived under a
		// different session label than the one the confirmation was registered
		// under. Resolved by ConfirmationId+owner above; record for observability.
		log.Printf("confirm %s: session drift (registered %q, resolved %q) — resolving by id+owner",
			confirmationID, registeredSession, sessionID)
	}
	return func() {
		p.ch <- decision
		close(p.ch)
	}, nil
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
// is cancelled (SSE disconnect), or the timeout expires. It preserves the
// historical decision-only API for callers that do not need attribution.
func WaitForConfirmation(ctx context.Context, ch <-chan ConfirmDecision, timeout time.Duration) ConfirmDecision {
	decision, _ := WaitForConfirmationOutcome(ctx, ch, timeout)
	return decision
}

// WaitForConfirmationOutcome preserves the terminal cause that the old boolean
// confirmation contract intentionally collapsed. The reason is a closed-set
// observability value: no broker id, form value, user text, or transport error
// reaches the trace. A closed channel without a resolved decision means the
// broker removed the interaction; a cancelled context means the client went
// away before resolving it.
func WaitForConfirmationOutcome(ctx context.Context, ch <-chan ConfirmDecision, timeout time.Duration) (ConfirmDecision, string) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case decision, ok := <-ch:
		if !ok {
			return ConfirmDecision{}, observability.ConfirmationReasonBrokerCancelled
		}
		if decision.Confirmed {
			return decision, observability.ConfirmationReasonUserConfirmed
		}
		return decision, observability.ConfirmationReasonUserDeclined
	case <-ctx.Done():
		return ConfirmDecision{}, observability.ConfirmationReasonClientDisconnect
	case <-timer.C:
		return ConfirmDecision{}, observability.ConfirmationReasonTimeout
	}
}
