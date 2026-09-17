package ws

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Deadlines bound a single outbound operation. A frame that cannot be written
// within Frame, or a ping whose pong does not arrive within Pong, means the
// peer is not consuming the connection; the Writer then reports a stall once.
type Deadlines struct {
	Frame time.Duration
	Pong  time.Duration
}

// Writer serializes outbound frames to a WebSocket connection through the
// WriteEvent / WriteKeepalive contract the Chat streaming core writes to.
//
// Each frame is a single JSON text message of the form {"event": <name>, ...data}
// — the data struct's top-level fields are flattened alongside a discriminating
// "event" tag. The frontend (frame/src/Frame/AIAssistant/service.js) switches on
// f.event, so the key MUST be "event" (not "type"). Writes are mutex-guarded
// because the Chat goroutine (tokens/steps) and the connection read loop
// (error frames) write concurrently; coder/websocket permits only one writer at
// a time, and Ping shares that same write side — so keepalive takes the lock too.
//
// Every write and ping runs under its own deadline rather than the connection
// lifetime. The lifetime is sized for a whole turn (tens of minutes); a peer that
// vanished without closing its socket would otherwise hold the write mutex — a
// ping waits for a pong that never comes — and stall every later frame until the
// lifetime expired, while the turn's work sat finished and unrecorded.
type Writer struct {
	conn      *websocket.Conn
	ctx       context.Context
	deadlines Deadlines
	onStall   func()
	mu        sync.Mutex
	stallOnce sync.Once
}

// New creates a WebSocket Writer bound to conn. ctx scopes every write and the
// keepalive ping to the connection lifetime; deadlines bound each operation
// inside that lifetime. onStall runs at most once, the first time an operation
// fails while the connection is otherwise still alive, so the owner can end the
// turn instead of waiting for the lifetime to run out.
func New(ctx context.Context, conn *websocket.Conn, deadlines Deadlines, onStall func()) *Writer {
	return &Writer{conn: conn, ctx: ctx, deadlines: deadlines, onStall: onStall}
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
	ctx, cancel := w.bounded(w.deadlines.Frame)
	defer cancel()
	// coder/websocket closes the connection itself when this context expires
	// mid-frame, so a stalled write also unblocks the connection's read loop.
	if err := w.conn.Write(ctx, websocket.MessageText, frame); err != nil {
		w.noteStall()
		return err
	}
	return nil
}

// WriteKeepalive sends a WebSocket ping. coder/websocket auto-handles the pong
// at the client. Ping shares the connection's single write side with Write, so
// it must hold the same mutex — otherwise a concurrent token write corrupts the
// frame stream. Unlike a frame write, a ping that times out waiting for its pong
// leaves the connection open; the stall report is what ends it.
func (w *Writer) WriteKeepalive() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	ctx, cancel := w.bounded(w.deadlines.Pong)
	defer cancel()
	if err := w.conn.Ping(ctx); err != nil {
		w.noteStall()
		return err
	}
	return nil
}

func (w *Writer) bounded(d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(w.ctx)
	}
	return context.WithTimeout(w.ctx, d)
}

// noteStall reports the first failure that happened while the connection was
// still supposed to be alive. A failure after the owner already ended the
// connection is the expected shutdown, not a stall.
func (w *Writer) noteStall() {
	if w.onStall == nil || w.ctx.Err() != nil {
		return
	}
	w.stallOnce.Do(w.onStall)
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
