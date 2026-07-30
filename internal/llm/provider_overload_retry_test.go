package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/compshare-agent/internal/config"
	openai "github.com/sashabaranov/go-openai"
)

// overloadServer serves `status` for the first `failures` requests and a
// one-token stream afterwards, counting attempts.
func overloadServer(t *testing.T, status int, body string, failures int32) (*httptest.Server, *int32) {
	t.Helper()
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) <= failures {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"recovered\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv, &attempts
}

func overloadChat(t *testing.T, srv *httptest.Server) (*ChatResponse, error) {
	t.Helper()
	client := NewClient(config.LLMConfig{BaseURL: srv.URL + "/v1", APIKey: "test-key", Model: "test-model"})
	return client.Chat(context.Background(), ChatRequest{
		Messages: []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hello"}},
	})
}

// A capacity 503 is the provider saying "not now", not "your request is wrong".
// It killed a real 29-step create turn on 2026-07-29 — the write had already
// committed and only the closing narration call hit the exhausted account pool;
// a probe with the same key and model answered 200 minutes later. One retry
// recovers the whole turn.
func TestChatRetriesProviderOverloadAndRecovers(t *testing.T) {
	const body = `{"error":{"message":"No available accounts. Please add accounts or check account status.","type":"server_error"}}`
	srv, attempts := overloadServer(t, http.StatusServiceUnavailable, body, 1)

	resp, err := overloadChat(t, srv)
	if err != nil {
		t.Fatalf("Chat error = %v, want recovery on the second attempt", err)
	}
	if resp.Content != "recovered" {
		t.Fatalf("Content = %q, want recovered", resp.Content)
	}
	if got := atomic.LoadInt32(attempts); got != 2 {
		t.Fatalf("attempts = %d, want 2 (one retry)", got)
	}
}

// When the pool is still empty on the retry the caller must still see the
// error — the retry buys one chance, it does not hide a persistent outage.
func TestChatSurfacesProviderOverloadThatPersists(t *testing.T) {
	srv, attempts := overloadServer(t, http.StatusServiceUnavailable, `{"error":{"message":"No available accounts."}}`, 99)

	if _, err := overloadChat(t, srv); err == nil {
		t.Fatal("Chat error = nil, want the persistent 503 surfaced")
	}
	if got := atomic.LoadInt32(attempts); got != 2 {
		t.Fatalf("attempts = %d, want 2 (retried once, then gave up)", got)
	}
}

// Which statuses mean "try again" is the whole decision here: retrying a
// deterministic rejection pays for the same refusal twice and delays the error
// the user needs to see, while NOT retrying a capacity blip throws away a turn
// that would have succeeded.
func TestProviderOverloadRetryIsScopedToCapacityStatuses(t *testing.T) {
	for _, tc := range []struct {
		status       int
		wantAttempts int32
	}{
		{http.StatusTooManyRequests, 2},
		{http.StatusInternalServerError, 2},
		{http.StatusServiceUnavailable, 2},
		{http.StatusBadRequest, 1},
		{http.StatusUnauthorized, 1},
		{http.StatusNotFound, 1},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			srv, attempts := overloadServer(t, tc.status, `{"error":{"message":"boom"}}`, 99)
			if _, err := overloadChat(t, srv); err == nil {
				t.Fatalf("status %d: Chat error = nil, want error", tc.status)
			}
			if got := atomic.LoadInt32(attempts); got != tc.wantAttempts {
				t.Fatalf("status %d: attempts = %d, want %d", tc.status, got, tc.wantAttempts)
			}
		})
	}
}

// The status must be read BEFORE the message keywords. A 4xx that merely says
// the word "timeout" is a rejection of the request, and the pre-2026-07-29
// classifier retried it purely because "timeout" appeared in the body — paying
// twice for an error that could never succeed.
func TestProviderRejectionMentioningTimeoutIsNotRetried(t *testing.T) {
	const body = `{"error":{"message":"request timeout is not a valid parameter","type":"invalid_request_error"}}`
	srv, attempts := overloadServer(t, http.StatusBadRequest, body, 99)

	if _, err := overloadChat(t, srv); err == nil {
		t.Fatal("Chat error = nil, want the 400 surfaced")
	}
	if got := atomic.LoadInt32(attempts); got != 1 {
		t.Fatalf("attempts = %d, want 1 — a 400 is a rejection even when it says \"timeout\"", got)
	}
}
