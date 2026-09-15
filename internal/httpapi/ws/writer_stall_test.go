package ws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

// stallHarness accepts one WebSocket, wraps it in a Writer with the given
// deadlines, and reports the first stall on a channel. The client side is
// returned to the test so it can decide whether to consume the connection.
type stallHarness struct {
	client  *websocket.Conn
	writer  chan *Writer
	stalled chan struct{}
}

func newStallHarness(t *testing.T, deadlines Deadlines) *stallHarness {
	t.Helper()
	h := &stallHarness{writer: make(chan *Writer, 1), stalled: make(chan struct{}, 1)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer conn.CloseNow()
		h.writer <- New(ctx, conn, deadlines, func() { h.stalled <- struct{}{} })
		// Keep the server side reading so pongs are processed, exactly as the
		// production read loop does while a turn is in flight.
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.CloseNow() })
	h.client = client
	return h
}

// A peer that never reads never answers a ping. The keepalive must give up
// within the pong deadline and report the stall once, instead of holding the
// write mutex for the connection lifetime.
func TestWriter_KeepaliveToASilentPeerReportsAStallWithinTheDeadline(t *testing.T) {
	h := newStallHarness(t, Deadlines{Frame: time.Second, Pong: 200 * time.Millisecond})
	writer := <-h.writer

	started := time.Now()
	err := writer.WriteKeepalive()
	elapsed := time.Since(started)

	require.Error(t, err, "a ping with no pong must fail")
	require.Less(t, elapsed, 2*time.Second, "the failure must come from the pong deadline, not the connection lifetime")
	select {
	case <-h.stalled:
	case <-time.After(time.Second):
		t.Fatal("the stall was not reported")
	}
	// A second failure is the same stall, not a new one.
	_ = writer.WriteKeepalive()
	select {
	case <-h.stalled:
		t.Fatal("a stall must be reported once")
	case <-time.After(100 * time.Millisecond):
	}
}

// The control: a peer that consumes the connection answers pings and receives
// frames, and nothing reports a stall. Without this the test above could pass
// against a Writer whose ping always fails.
func TestWriter_KeepaliveToAConsumingPeerSucceeds(t *testing.T) {
	h := newStallHarness(t, Deadlines{Frame: time.Second, Pong: 200 * time.Millisecond})
	writer := <-h.writer
	received := make(chan []byte, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for {
			_, data, err := h.client.Read(ctx)
			if err != nil {
				return
			}
			received <- data
		}
	}()

	require.NoError(t, writer.WriteKeepalive())
	require.NoError(t, writer.WriteEvent("token", map[string]any{"Text": "hi"}))

	select {
	case data := <-received:
		require.Contains(t, string(data), `"event":"token"`)
	case <-time.After(time.Second):
		t.Fatal("the frame did not reach a consuming peer")
	}
	select {
	case <-h.stalled:
		t.Fatal("a consuming peer must not be reported as stalled")
	case <-time.After(100 * time.Millisecond):
	}
}

// Once the owner has ended the connection, every later failure is the expected
// shutdown: the stall hook must not fire for it.
func TestWriter_FailureAfterTheConnectionEndedIsNotAStall(t *testing.T) {
	stalled := false
	conn := &websocket.Conn{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := New(ctx, conn, Deadlines{Frame: time.Second, Pong: time.Second}, func() { stalled = true })

	w.noteStall()

	require.False(t, stalled)
}
