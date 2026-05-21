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

// countingLLM wraps stubLLMReply behavior but exposes a call count for
// the quota tests, which need to assert "the denied turn did NOT invoke
// the LLM" -- a guarantee that no other server test currently asserts.
type countingLLM struct {
	mu    sync.Mutex
	calls int
	reply string
}

func (c *countingLLM) Chat(_ context.Context, _ llm.ChatRequest) (*llm.ChatResponse, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return &llm.ChatResponse{Content: c.reply}, nil
}

func (c *countingLLM) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// spyRateLimiter wraps a real limiter and records every Allow request so
// tests can assert WHICH classes did and did not flow through it. Used to
// prove that confirm_response and ping frames do NOT consume the
// ClassUserTurn quota -- the (a) constraint from 2026-05-21 review.
type spyRateLimiter struct {
	inner governance.RateLimiter

	mu       sync.Mutex
	requests []governance.Request
}

func (s *spyRateLimiter) Allow(req governance.Request) governance.Decision {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()
	return s.inner.Allow(req)
}

func (s *spyRateLimiter) countByClass(class governance.Class) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, req := range s.requests {
		if req.Class == class {
			n++
		}
	}
	return n
}

func newQuotaTestDeps(t *testing.T, userTurnDaily int) (*engine.SharedDeps, *countingLLM, *spyRateLimiter) {
	t.Helper()
	limits := governance.DefaultLimits()
	limits.UserTurnDaily = userTurnDaily
	spy := &spyRateLimiter{inner: governance.NewMemoryLimiter(limits)}

	stub := &countingLLM{reply: "ok"}
	deps := &engine.SharedDeps{
		LLMClient:                stub,
		RateLimiter:              spy,
		SupportsObjectToolChoice: true,
		ExternalExecutor:         stubExecutor{},
	}
	return deps, stub, spy
}

// TestRunChatTurn_PingDoesNotConsumeUserTurnQuota -- (a) constraint guard.
//
// Encodes WHY: pre-review the rate-limit check could have been placed on
// every reader-dispatched frame, which would let a single mutating-tool
// confirmation (potentially several confirm_response frames if the user
// retries) burn through a user's daily turn budget. Test pumps 3 pings
// and asserts the limiter never saw a ClassUserTurn Allow call -- only
// ClientMsgUserMessage may consume that bucket.
func TestRunChatTurn_PingDoesNotConsumeUserTurnQuota(t *testing.T) {
	deps, _, spy := newQuotaTestDeps(t, 5)
	baseURL, shutdown := startTestServer(t, deps)
	defer shutdown()

	conn := dialWSWithTenant(t, baseURL, 11, 111)
	defer conn.Close(websocket.StatusNormalClosure, "")

	waitForType(t, conn, ServerMsgReady, 5*time.Second)

	// Pump 3 pings. None should hit the ClassUserTurn limiter.
	for i := 0; i < 3; i++ {
		if err := wsjson.Write(context.Background(), conn, ClientMessage{Type: ClientMsgPing}); err != nil {
			t.Fatalf("write ping #%d: %v", i, err)
		}
		waitForType(t, conn, ServerMsgPong, 2*time.Second)
	}

	if got := spy.countByClass(governance.ClassUserTurn); got != 0 {
		t.Fatalf("pings must not consume ClassUserTurn quota; got %d Allow calls", got)
	}

	// Now send a single user_message and verify exactly one ClassUserTurn
	// Allow call appears -- proving the limiter IS engaged on the right
	// dispatch path (not just disabled).
	if err := wsjson.Write(context.Background(), conn, ClientMessage{
		Type: ClientMsgUserMessage, Text: "hi", RequestUUID: "uuid-1",
	}); err != nil {
		t.Fatalf("write user_message: %v", err)
	}
	waitForType(t, conn, ServerMsgAnswerFinal, 10*time.Second)
	waitForType(t, conn, ServerMsgDone, 5*time.Second)

	if got := spy.countByClass(governance.ClassUserTurn); got != 1 {
		t.Fatalf("expected exactly 1 ClassUserTurn Allow call after user_message; got %d", got)
	}
}

// TestRunChatTurn_UserTurnDailyCapExhausted_ReturnsRateLimited -- integration
// test for the test-phase 30/day guardrail.
//
// Encodes WHY: end-to-end coverage that (1) the Allow call lives at the
// runChatTurn entry, (2) ErrCodeRateLimited is emitted with a Done frame
// to terminate the turn cleanly, (3) no engine.Chat call leaks through
// on a denial. With UserTurnDaily=1 and two pumped user_messages, only
// the first reaches the LLM; the second comes back as rate_limited.
func TestRunChatTurn_UserTurnDailyCapExhausted_ReturnsRateLimited(t *testing.T) {
	deps, stub, _ := newQuotaTestDeps(t, 1)
	baseURL, shutdown := startTestServer(t, deps)
	defer shutdown()

	conn := dialWSWithTenant(t, baseURL, 11, 111)
	defer conn.Close(websocket.StatusNormalClosure, "")

	waitForType(t, conn, ServerMsgReady, 5*time.Second)

	// Turn 1: allowed, LLM called, normal completion.
	if err := wsjson.Write(context.Background(), conn, ClientMessage{
		Type: ClientMsgUserMessage, Text: "first", RequestUUID: "uuid-1",
	}); err != nil {
		t.Fatalf("write turn-1: %v", err)
	}
	waitForType(t, conn, ServerMsgAnswerFinal, 10*time.Second)
	waitForType(t, conn, ServerMsgDone, 5*time.Second)

	llmCallsAfterTurn1 := stub.callCount()
	if llmCallsAfterTurn1 != 1 {
		t.Fatalf("turn 1 should have produced exactly 1 LLM call; got %d", llmCallsAfterTurn1)
	}

	// Turn 2: cap=1 already consumed, must return rate_limited + done.
	if err := wsjson.Write(context.Background(), conn, ClientMessage{
		Type: ClientMsgUserMessage, Text: "second", RequestUUID: "uuid-2",
	}); err != nil {
		t.Fatalf("write turn-2: %v", err)
	}

	// Drain frames until we see Error+rate_limited+uuid-2 AND Done+uuid-2.
	// Any other frame types BEFORE these are tolerated as long as they
	// belong to turn 2 -- we don't want to be brittle to future additions
	// like a server_ack frame. The strong invariant we DO test is the
	// LLM call count below: no engine.Chat may run for a denied turn.
	deadline := time.Now().Add(5 * time.Second)
	readCtx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	var sawRateLimited, sawDone bool
	for time.Now().Before(deadline) && !(sawRateLimited && sawDone) {
		var msg ServerMessage
		if err := wsjson.Read(readCtx, conn, &msg); err != nil {
			t.Fatalf("read while waiting for rate_limited: %v", err)
		}
		if msg.RequestUUID != "uuid-2" {
			t.Fatalf("turn 2 frames must carry uuid-2; got %+v", msg)
		}
		switch msg.Type {
		case ServerMsgError:
			if msg.Code != ErrCodeRateLimited {
				t.Fatalf("expected rate_limited error code, got %q (%s)", msg.Code, msg.Message)
			}
			sawRateLimited = true
		case ServerMsgDone:
			sawDone = true
			// Any other frame types are tolerated; the LLM call-count
			// invariant below is the load-bearing assertion.
		}
	}
	if !sawRateLimited || !sawDone {
		t.Fatalf("turn 2 expected rate_limited + done; sawRateLimited=%v sawDone=%v",
			sawRateLimited, sawDone)
	}

	// The denied turn must NOT have invoked the LLM.
	if got := stub.callCount(); got != llmCallsAfterTurn1 {
		t.Fatalf("denied turn must not invoke LLM; calls went from %d to %d",
			llmCallsAfterTurn1, got)
	}
}
