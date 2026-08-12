package httpapi

import (
	"testing"

	"github.com/compshare-agent/internal/store"
	"github.com/stretchr/testify/require"
)

// A resolution must not wake the turn until the transport has acknowledged it.
//
// Resolve used to do both in one step, ending with the channel send that unblocks
// the chat goroutine. That goroutine can then finish the turn, write `done` and
// cancel the connection context — all before the read-loop goroutine reaches its
// WriteEvent — and the acknowledgement is dropped onto a closed socket. The
// client, told nothing, settles the card as "连接已关闭" for an action the server
// accepted and already executed.
//
// This is the honest gate for that ordering. The WS integration tests cannot be:
// they read the ack frame off the socket and would pass under either order.
func TestClaimResolutionDoesNotWakeTheTurnBeforeItIsDelivered(t *testing.T) {
	owner := store.Owner{TopOrganizationID: 1, OrganizationID: 2}
	b := NewConfirmBroker()
	id, ch := b.Register("sess-1", owner)

	deliver, err := b.ClaimResolution(id, "sess-1", owner, ConfirmDecision{Confirmed: true})
	require.NoError(t, err)

	select {
	case d := <-ch:
		t.Fatalf("the turn was woken before the transport could acknowledge (got %+v)", d)
	default:
	}

	// Claimed means claimed: the id is already out of the map, so nothing else can
	// deliver a second decision to the same waiter.
	_, again := b.ClaimResolution(id, "sess-1", owner, ConfirmDecision{Confirmed: false})
	require.ErrorIs(t, again, ErrConfirmationNotFound)

	deliver()
	require.True(t, (<-ch).Confirmed, "the claimed decision must reach the turn once delivered")
}

// The claim must reject before it consumes the card, so a refused resolve leaves
// the pending entry exactly as it was and the user can still answer it.
func TestClaimResolutionKeepsThePendingCardWhenItRefuses(t *testing.T) {
	owner := store.Owner{TopOrganizationID: 1, OrganizationID: 2}
	other := store.Owner{TopOrganizationID: 9, OrganizationID: 9}
	b := NewConfirmBroker()
	id, ch := b.Register("sess-1", owner)

	_, err := b.ClaimResolution(id, "sess-1", other, ConfirmDecision{Confirmed: true})
	require.ErrorIs(t, err, ErrConfirmationOwner)

	deliver, err := b.ClaimResolution(id, "sess-1", owner, ConfirmDecision{Confirmed: true})
	require.NoError(t, err, "a rejected claim must not have consumed the card")
	deliver()
	require.True(t, (<-ch).Confirmed)
}

// orderRecordingWriter notes the sequence of writes so a test can compare it
// against when the turn was woken.
type orderRecordingWriter struct {
	events *[]string
}

func (w orderRecordingWriter) WriteEvent(event string, _ any) error {
	*w.events = append(*w.events, "frame:"+event)
	return nil
}
func (w orderRecordingWriter) WriteKeepalive() error { return nil }

// The acknowledgement must be on the wire BEFORE the turn is woken.
//
// This is the assertion the WS integration tests cannot make: they read the ack
// off a socket and pass under either order. Here the waiting turn records the
// moment it is unblocked into the same sequence the writer records into, so the
// order is observable.
//
// Getting it backwards is not theoretical: the woken chat goroutine can finish
// the turn, write `done` and cancel the connection context before this goroutine
// reaches its WriteEvent, dropping the ack onto a closed socket. The client then
// reports "连接已关闭" for an action the server accepted and already executed.
func TestConfirmFrameAcknowledgesBeforeWakingTheTurn(t *testing.T) {
	owner := store.Owner{TopOrganizationID: 1, OrganizationID: 2}
	h := &Handlers{confirmBroker: NewConfirmBroker()}
	id, ch := h.confirmBroker.Register("sess-1", owner)

	var events []string
	woken := make(chan struct{})
	go func() {
		<-ch
		events = append(events, "turn:woken")
		close(woken)
	}()

	h.resolveConfirmFrame(orderRecordingWriter{events: &events}, id, "sess-1", owner,
		ConfirmDecision{Confirmed: true})
	<-woken

	require.Equal(t, []string{"frame:confirmation_ack", "turn:woken"}, events,
		"the turn must not be woken until the client has been told its answer was accepted")
}

// A refused resolve wakes nothing at all — the card stays pending for its own
// timeout, and the client gets a card-scoped error rather than a dead turn.
func TestConfirmFrameRefusalWakesNothing(t *testing.T) {
	owner := store.Owner{TopOrganizationID: 1, OrganizationID: 2}
	h := &Handlers{confirmBroker: NewConfirmBroker()}
	id, ch := h.confirmBroker.Register("sess-1", owner)

	var events []string
	h.resolveConfirmFrame(orderRecordingWriter{events: &events}, "some-other-id", "sess-1", owner,
		ConfirmDecision{Confirmed: true})

	require.Equal(t, []string{"frame:error"}, events)
	select {
	case d := <-ch:
		t.Fatalf("a rejected resolve must not reach the waiting turn (got %+v)", d)
	default:
	}
	// And the real card is untouched: the user can still answer it.
	require.True(t, func() bool {
		deliver, err := h.confirmBroker.ClaimResolution(id, "sess-1", owner, ConfirmDecision{Confirmed: true})
		if err != nil {
			return false
		}
		deliver()
		return (<-ch).Confirmed
	}())
}
