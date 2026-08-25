package httpapi

import (
	"context"
	"sync"
)

type activeWebSocket struct {
	cancel context.CancelFunc
	done   <-chan struct{}
}

// websocketLifecycle tracks only upgraded connections. Ordinary HTTP requests
// remain owned by net/http.Server.Shutdown. Once draining begins, registration
// is permanently closed because the owning server is terminating.
type websocketLifecycle struct {
	mu       sync.Mutex
	draining bool
	nextID   uint64
	active   map[uint64]activeWebSocket
}

func (h *Handlers) registerWebSocket(cancel context.CancelFunc, done <-chan struct{}) (uint64, bool) {
	if h == nil || cancel == nil || done == nil {
		return 0, false
	}
	lifecycle := &h.wsLifecycle
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.draining {
		cancel()
		return 0, false
	}
	if lifecycle.active == nil {
		lifecycle.active = make(map[uint64]activeWebSocket)
	}
	lifecycle.nextID++
	id := lifecycle.nextID
	lifecycle.active[id] = activeWebSocket{cancel: cancel, done: done}
	return id, true
}

func (h *Handlers) unregisterWebSocket(id uint64) {
	if h == nil || id == 0 {
		return
	}
	h.wsLifecycle.mu.Lock()
	delete(h.wsLifecycle.active, id)
	h.wsLifecycle.mu.Unlock()
}

// ShutdownWebSockets cancels every active chat turn and waits for the owning
// handlers to return. HandleWS waits for its chat goroutine, whose terminus
// persists the background-job cursor before releasing the session lease.
func (h *Handlers) ShutdownWebSockets(ctx context.Context) error {
	if h == nil {
		return nil
	}
	lifecycle := &h.wsLifecycle
	lifecycle.mu.Lock()
	lifecycle.draining = true
	active := make([]activeWebSocket, 0, len(lifecycle.active))
	for _, connection := range lifecycle.active {
		active = append(active, connection)
	}
	lifecycle.mu.Unlock()

	for _, connection := range active {
		connection.cancel()
	}
	for _, connection := range active {
		select {
		case <-connection.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
