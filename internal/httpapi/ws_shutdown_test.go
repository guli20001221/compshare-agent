package httpapi

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestShutdownWebSocketsCancelsAndWaitsForActiveHandlers(t *testing.T) {
	h := &Handlers{}
	turnCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	id, accepted := h.registerWebSocket(cancel, done)
	require.True(t, accepted)

	returned := make(chan error, 1)
	go func() {
		returned <- h.ShutdownWebSockets(context.Background())
	}()
	select {
	case <-turnCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("active WebSocket turn was not cancelled")
	}
	select {
	case <-returned:
		t.Fatal("shutdown returned before the handler persistence path completed")
	default:
	}
	close(done)
	require.NoError(t, <-returned)
	h.unregisterWebSocket(id)

	lateCtx, lateCancel := context.WithCancel(context.Background())
	lateDone := make(chan struct{})
	_, accepted = h.registerWebSocket(lateCancel, lateDone)
	require.False(t, accepted, "a terminating server must not admit a new hijacked connection")
	require.Error(t, lateCtx.Err())
}

func TestShutdownWebSocketsHonorsDeadline(t *testing.T) {
	h := &Handlers{}
	_, accepted := h.registerWebSocket(func() {}, make(chan struct{}))
	require.True(t, accepted)
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	require.ErrorIs(t, h.ShutdownWebSockets(ctx), context.DeadlineExceeded)
}
