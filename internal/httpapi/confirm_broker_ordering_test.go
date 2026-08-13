package httpapi

import (
	"errors"
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

// orderRecordingWriter observes, at the moment of each write, whether the
// decision has ALREADY been handed to the waiting turn.
//
// It peeks at the waiter's channel instead of racing a second goroutine against
// it. The first version of this test did exactly that and was an empty gate: the
// pending channel is buffered (cap 1), so deliver() returns without blocking and
// without synchronizing, and the reader goroutine had usually not been scheduled
// by the time the writer recorded — so swapping the two lines under test changed
// nothing observable. Peeking is single-goroutine and deterministic.
type orderRecordingWriter struct {
	events *[]string
	ch     <-chan ConfirmDecision
	seen   *ConfirmDecision
}

func (w orderRecordingWriter) WriteEvent(event string, _ any) error {
	select {
	case d := <-w.ch:
		*w.seen = d
		*w.events = append(*w.events, "turn:woken")
	default:
	}
	*w.events = append(*w.events, "frame:"+event)
	return nil
}
func (w orderRecordingWriter) WriteKeepalive() error { return nil }

// The acknowledgement must be on the wire BEFORE the turn is woken.
//
// This is the assertion the WS integration tests cannot make: they read the ack
// off a socket and pass under either order.
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
	var seen ConfirmDecision
	cancelled := false
	h.resolveConfirmFrame(orderRecordingWriter{events: &events, ch: ch, seen: &seen},
		func() { cancelled = true }, id, "sess-1", owner,
		ConfirmDecision{Confirmed: true})

	require.Equal(t, []string{"frame:confirmation_ack"}, events,
		"the turn must not hold the decision yet when the acknowledgement is written")
	require.False(t, cancelled, "a delivered acknowledgement must not end the connection")
	require.True(t, (<-ch).Confirmed, "the decision must still reach the turn afterwards")
}

// failingAckWriter accepts every frame except the acknowledgement — the shape of
// a socket that died between the confirm frame arriving and the answer going out
// (the user closed the drawer, the gateway dropped the connection).
type failingAckWriter struct{ events *[]string }

func (w failingAckWriter) WriteEvent(event string, _ any) error {
	*w.events = append(*w.events, event)
	if event == "confirmation_ack" {
		return errors.New("failed to write: use of closed network connection")
	}
	return nil
}
func (w failingAckWriter) WriteKeepalive() error { return nil }

// An acknowledgement that cannot be written must NOT execute the action.
//
// The claim already happened, so it is tempting to treat the write as
// best-effort and deliver anyway. That produces the one outcome this whole
// change exists to prevent, only worse: the client waits out CONFIRM_ACK_TIMEOUT_MS
// and reports that the server never answered, while the agent SSHes into the box
// and runs the authorized command against a socket nobody is reading. Fail-closed
// instead — drop the decision, end the connection, and let the waiting turn
// resolve as client_disconnect.
func TestConfirmFrameDoesNotExecuteWhenTheAcknowledgementCannotBeWritten(t *testing.T) {
	owner := store.Owner{TopOrganizationID: 1, OrganizationID: 2}
	h := &Handlers{confirmBroker: NewConfirmBroker()}
	id, ch := h.confirmBroker.Register("sess-1", owner)

	var events []string
	cancelled := false
	h.resolveConfirmFrame(failingAckWriter{events: &events}, func() { cancelled = true },
		id, "sess-1", owner, ConfirmDecision{Confirmed: true})

	require.Equal(t, []string{"confirmation_ack"}, events)
	select {
	case d := <-ch:
		t.Fatalf("an unacknowledged confirmation must not reach the turn (got %+v)", d)
	default:
	}
	require.True(t, cancelled,
		"the turn must be ended, not left waiting out its own timeout on a dead socket")
}

// A refused resolve wakes nothing at all — the card stays pending for its own
// timeout, and the client gets a card-scoped error rather than a dead turn.
func TestConfirmFrameRefusalWakesNothing(t *testing.T) {
	owner := store.Owner{TopOrganizationID: 1, OrganizationID: 2}
	h := &Handlers{confirmBroker: NewConfirmBroker()}
	id, ch := h.confirmBroker.Register("sess-1", owner)

	var events []string
	var seen ConfirmDecision
	cancelled := false
	h.resolveConfirmFrame(orderRecordingWriter{events: &events, ch: ch, seen: &seen},
		func() { cancelled = true }, "some-other-id", "sess-1", owner,
		ConfirmDecision{Confirmed: true})

	require.Equal(t, []string{"frame:error"}, events)
	require.False(t, cancelled,
		"a card-scoped refusal must leave the connection open — the turn is still running")
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
