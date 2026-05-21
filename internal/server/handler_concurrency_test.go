package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/llm"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// blockingLLM is an LLMClient that blocks every Chat call on the
// `release` channel. Tests use it to hold an in-flight engine.Chat()
// open while exercising the reader path.
type blockingLLM struct {
	release chan struct{}
	reply   string

	mu       sync.Mutex
	inFlight int
}

func newBlockingLLM(reply string) *blockingLLM {
	return &blockingLLM{release: make(chan struct{}), reply: reply}
}

func (b *blockingLLM) Chat(ctx context.Context, _ llm.ChatRequest) (*llm.ChatResponse, error) {
	b.mu.Lock()
	b.inFlight++
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.inFlight--
		b.mu.Unlock()
	}()
	select {
	case <-b.release:
		return &llm.ChatResponse{Content: b.reply}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *blockingLLM) waitForInFlight(t *testing.T, n int, deadline time.Duration) {
	t.Helper()
	dl := time.Now().Add(deadline)
	for time.Now().Before(dl) {
		b.mu.Lock()
		got := b.inFlight
		b.mu.Unlock()
		if got == n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("blockingLLM never reached inFlight=%d within %v", n, deadline)
}

func newBlockingTestDeps(reply string) (*engine.SharedDeps, *blockingLLM) {
	llm := newBlockingLLM(reply)
	deps := &engine.SharedDeps{
		LLMClient:                llm,
		RateLimiter:              governance.NewMemoryLimiter(governance.DefaultLimits()),
		SupportsObjectToolChoice: true,
		ExternalExecutor:         stubExecutor{},
	}
	return deps, llm
}

// TestHandleWS_ReaderNotBlockedByChat — PR10 architectural contract.
//
// Encodes WHY: pre-PR10 readerLoop called runChatTurn directly, so a
// Chat in progress blocked the reader. When that Chat was waiting on
// confirmFn for the client's confirm_response frame, the reader could
// not deliver that very frame — a structural deadlock that pre-PR10
// timed out at confirm_bridge's 30s defaultConfirmTimeout. PR10 split
// reader and chat into separate goroutines connected by a turnQueue.
//
// This test asserts the architectural invariant without needing to wire
// a full confirm path: with a deliberately-stuck Chat, the reader must
// still process unrelated frames (ping → pong) promptly. If the reader
// is blocked on chat, the pong takes ≥ the LLM hold time. If PR10 split
// is intact, pong arrives well under 1s.
func TestHandleWS_ReaderNotBlockedByChat(t *testing.T) {
	deps, blocking := newBlockingTestDeps("eventual reply")
	baseURL, shutdown := startTestServer(t, deps)
	defer shutdown()

	conn := dialWSWithTenant(t, baseURL, 11, 111)
	defer conn.Close(websocket.StatusNormalClosure, "")

	waitForType(t, conn, ServerMsgReady, 5*time.Second)

	// Send user_message → chatLoop picks it up → engine.Chat blocks on
	// blockingLLM.release.
	if err := wsjson.Write(context.Background(), conn, ClientMessage{
		Type:        ClientMsgUserMessage,
		Text:        "hi",
		RequestUUID: "uuid-1",
	}); err != nil {
		t.Fatalf("write user_message: %v", err)
	}

	// Confirm Chat actually entered the LLM call before we test the
	// reader's responsiveness — otherwise a slow chatLoop schedule could
	// give a false positive.
	blocking.waitForInFlight(t, 1, 5*time.Second)

	// Now send a ping. Pre-PR10 the reader is stuck inside runChatTurn
	// and can't read this frame; pong never comes back until Chat
	// returns. Post-PR10 the reader is independent and pong returns fast.
	if err := wsjson.Write(context.Background(), conn, ClientMessage{Type: ClientMsgPing}); err != nil {
		t.Fatalf("write ping: %v", err)
	}

	pongDeadline := time.Now().Add(2 * time.Second)
	pongCtx, cancel := context.WithDeadline(context.Background(), pongDeadline)
	defer cancel()
	var pong ServerMessage
	if err := wsjson.Read(pongCtx, conn, &pong); err != nil {
		t.Fatalf("reader did not deliver pong within 2s — PR10 split broken: %v", err)
	}
	if pong.Type != ServerMsgPong {
		t.Fatalf("expected pong while chat is blocked, got %q", pong.Type)
	}

	// Release the LLM and let Chat complete to keep the test
	// infrastructure clean.
	close(blocking.release)
	waitForType(t, conn, ServerMsgAnswerFinal, 10*time.Second)
	waitForType(t, conn, ServerMsgDone, 5*time.Second)
}

// TestHandleWS_SecondUserMessageWhileTurnInProgress — PR10 buffer=1 contract.
//
// Encodes WHY: turnQueue buffer=1 + default→error is intentional. A
// client that fires user_message faster than chatLoop can process is
// violating the "one turn at a time" protocol; the server surfaces
// protocol_violation instead of silently dropping or unbounded-buffering
// (which would defeat per-turn ordering guarantees and could starve
// memory under a malicious client). This test pumps two messages while
// the first is still mid-Chat and asserts the second comes back as
// error/protocol_violation, not silently merged or queued indefinitely.
func TestHandleWS_SecondUserMessageWhileTurnInProgress(t *testing.T) {
	deps, blocking := newBlockingTestDeps("reply-1")
	baseURL, shutdown := startTestServer(t, deps)
	defer shutdown()

	conn := dialWSWithTenant(t, baseURL, 11, 111)
	defer conn.Close(websocket.StatusNormalClosure, "")

	waitForType(t, conn, ServerMsgReady, 5*time.Second)

	// First user_message — will hold inside engine.Chat until release.
	if err := wsjson.Write(context.Background(), conn, ClientMessage{
		Type: ClientMsgUserMessage, Text: "first", RequestUUID: "uuid-1",
	}); err != nil {
		t.Fatalf("write first user_message: %v", err)
	}
	blocking.waitForInFlight(t, 1, 5*time.Second)

	// Buffer is 1. Pump TWO additional messages: the second fills the
	// buffer, the third must hit the default→error branch. We pump both
	// because the protocol_violation only fires when the buffer is also
	// full; we don't want the test sensitive to whether the buffer "next
	// to dequeue" slot has filled yet.
	if err := wsjson.Write(context.Background(), conn, ClientMessage{
		Type: ClientMsgUserMessage, Text: "second", RequestUUID: "uuid-2",
	}); err != nil {
		t.Fatalf("write second user_message: %v", err)
	}
	if err := wsjson.Write(context.Background(), conn, ClientMessage{
		Type: ClientMsgUserMessage, Text: "third", RequestUUID: "uuid-3",
	}); err != nil {
		t.Fatalf("write third user_message: %v", err)
	}

	// Read frames until we see the protocol_violation error or hit
	// timeout. The frame may come back as soon as the reader processes
	// it (well under 1s); we allow 2s for scheduling slack.
	deadline := time.Now().Add(2 * time.Second)
	readCtx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	var sawViolation bool
	for time.Now().Before(deadline) && !sawViolation {
		var msg ServerMessage
		if err := wsjson.Read(readCtx, conn, &msg); err != nil {
			break
		}
		if msg.Type == ServerMsgError &&
			msg.Code == ErrCodeProtocolViolation &&
			msg.RequestUUID == "uuid-3" {
			sawViolation = true
		}
	}
	if !sawViolation {
		t.Fatalf("expected protocol_violation error for uuid-3 within 2s, never received")
	}

	// Cleanup: release the blocked chat so the connection can drain.
	close(blocking.release)
}
