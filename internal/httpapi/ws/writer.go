package ws

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/coder/websocket"
)

// Writer serializes outbound frames to a WebSocket connection. It satisfies the
// same WriteEvent / WriteKeepalive contract as the legacy sse.Writer so the
// Chat streaming core can write to either transport through one interface.
//
// Each frame is a single JSON text message of the form {"event": <name>, ...data}
// — the data struct's top-level fields are flattened alongside a discriminating
// "event" tag. The frontend (frame/src/Frame/AIAssistant/service.js) switches on
// f.event, so the key MUST be "event" (not "type"). Writes are mutex-guarded
// because the Chat goroutine (tokens/steps) and the connection read loop
// (error frames) write concurrently; coder/websocket permits only one writer at
// a time, and Ping shares that same write side — so keepalive takes the lock too.
type Writer struct {
	conn *websocket.Conn
	ctx  context.Context
	mu   sync.Mutex
}

// New creates a WebSocket Writer bound to conn. ctx scopes every write and the
// keepalive ping to the connection lifetime.
func New(ctx context.Context, conn *websocket.Conn) *Writer {
	return &Writer{conn: conn, ctx: ctx}
}

// WriteEvent marshals data, injects the "event" discriminator, and writes one
// text frame. A nil data produces a frame carrying only {"event": name}.
func (w *Writer) WriteEvent(event string, data any) error {
	frame, err := buildFrame(event, data)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.Write(w.ctx, websocket.MessageText, frame)
}

// WriteKeepalive sends a WebSocket ping. coder/websocket auto-handles the pong
// at the client. Ping shares the connection's single write side with Write, so
// it must hold the same mutex — otherwise a concurrent token write corrupts the
// frame stream.
func (w *Writer) WriteKeepalive() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn.Ping(w.ctx)
}

// buildFrame flattens data's top-level JSON fields next to an "event" tag. The
// "event" key always wins on collision.
func buildFrame(event string, data any) ([]byte, error) {
	m := map[string]any{}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
	}
	m["event"] = event
	return json.Marshal(m)
}
