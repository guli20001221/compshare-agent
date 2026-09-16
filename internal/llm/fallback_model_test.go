package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	openai "github.com/sashabaranov/go-openai"
)

const (
	primaryModel  = "gpt-5.6-terra"
	fallbackModel = "gpt-5.6-luna"
)

// wireRequest is the part of each request the tests dispatch on.
type wireRequest struct {
	Model         string          `json:"model"`
	StreamOptions json.RawMessage `json:"stream_options"`
}

// modelServer records the model of every request in order and lets each test
// decide the reply per request.
type modelServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []wireRequest
}

func newModelServer(t *testing.T, reply func(w http.ResponseWriter, req wireRequest, n int)) *modelServer {
	t.Helper()
	srv := &modelServer{}
	srv.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		var req wireRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		srv.mu.Lock()
		srv.requests = append(srv.requests, req)
		n := len(srv.requests)
		srv.mu.Unlock()
		reply(w, req, n)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *modelServer) models() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	models := make([]string, len(s.requests))
	for i, req := range s.requests {
		models[i] = req.Model
	}
	return models
}

func writeStatus(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":{"message":"` + message + `","type":"server_error"}}`))
}

func writeAnswer(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"" + text + "\"},\"finish_reason\":\"stop\"}]}\n\n"))
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
}

func newFallbackClient(srv *modelServer) *Client {
	return NewClient(config.LLMConfig{BaseURL: srv.URL + "/v1", APIKey: "test-key", Model: primaryModel, FallbackModel: fallbackModel})
}

func observedChat(t *testing.T, client *Client, req ChatRequest) (*ChatResponse, error, []OutboundCallResult) {
	t.Helper()
	var attempts []OutboundCallResult
	ctx := WithOutboundCallResultObserver(context.Background(), func(result OutboundCallResult) {
		attempts = append(attempts, result)
	})
	if len(req.Messages) == 0 {
		req.Messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hello"}}
	}
	resp, err := client.Chat(ctx, req)
	return resp, err, attempts
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The gateway fails per model pool and slowly, so the second request of a call
// goes to the other pool instead of back to the one that just failed. The trace
// shows the switch through the model on each attempt.
func TestChatRoutesToFallbackModelWhenPrimaryFailsUpstream(t *testing.T) {
	srv := newModelServer(t, func(w http.ResponseWriter, req wireRequest, _ int) {
		if req.Model == primaryModel {
			writeStatus(w, http.StatusServiceUnavailable, "No available accounts.")
			return
		}
		writeAnswer(w, "from-luna")
	})
	client := newFallbackClient(srv)

	resp, err, attempts := observedChat(t, client, ChatRequest{})
	if err != nil {
		t.Fatalf("Chat error = %v, want the fallback model's answer", err)
	}
	if resp.Content != "from-luna" {
		t.Fatalf("Content = %q, want from-luna", resp.Content)
	}
	if got := srv.models(); !sameStrings(got, []string{primaryModel, fallbackModel}) {
		t.Fatalf("requests went to %v, want primary then fallback", got)
	}
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(attempts))
	}
	if a := attempts[0]; a.Call.Model != primaryModel || a.AttemptInCall != 1 || a.Outcome != OutboundAttemptError ||
		a.ErrorClass != OutboundErrorUpstream5xx || !a.Retried {
		t.Fatalf("primary attempt trace = %#v", a)
	}
	if a := attempts[1]; a.Call.Model != fallbackModel || a.AttemptInCall != 2 || a.Outcome != OutboundAttemptSuccess || a.Retried {
		t.Fatalf("fallback attempt trace = %#v", a)
	}
}

// A call that just watched the primary fail must not make the next call pay
// the same wait: for primaryCooldown later calls start on the fallback, and
// the primary comes back into first place once the cooldown has lapsed.
func TestChatSkipsPrimaryWhileItIsCoolingDown(t *testing.T) {
	primaryDown := true
	srv := newModelServer(t, func(w http.ResponseWriter, req wireRequest, _ int) {
		if req.Model == primaryModel && primaryDown {
			writeStatus(w, http.StatusBadGateway, "upstream unresponsive")
			return
		}
		writeAnswer(w, "ok-"+req.Model)
	})
	client := newFallbackClient(srv)

	if _, err, _ := observedChat(t, client, ChatRequest{}); err != nil {
		t.Fatalf("first call error = %v", err)
	}
	resp, err, attempts := observedChat(t, client, ChatRequest{})
	if err != nil {
		t.Fatalf("second call error = %v", err)
	}
	if resp.Content != "ok-"+fallbackModel {
		t.Fatalf("second call Content = %q, want the fallback's answer without touching the primary", resp.Content)
	}
	if len(attempts) != 1 || attempts[0].Call.Model != fallbackModel || attempts[0].AttemptInCall != 1 {
		t.Fatalf("second call attempts = %#v, want one first-attempt request to the fallback", attempts)
	}
	if got := srv.models(); !sameStrings(got, []string{primaryModel, fallbackModel, fallbackModel}) {
		t.Fatalf("requests went to %v, want the primary skipped on the second call", got)
	}

	primaryDown = false
	client.primaryUnhealthyUntil.Store(time.Now().Add(-time.Second).UnixNano())
	resp, err, attempts = observedChat(t, client, ChatRequest{})
	if err != nil {
		t.Fatalf("third call error = %v", err)
	}
	if resp.Content != "ok-"+primaryModel || len(attempts) != 1 || attempts[0].Call.Model != primaryModel {
		t.Fatalf("after the cooldown the primary must be first again: Content = %q, attempts = %#v", resp.Content, attempts)
	}
}

// While the primary cools down the fallback goes first, but a fallback that
// also fails upstream still leaves the primary as the last resort.
func TestChatKeepsPrimaryAsLastResortWhileCoolingDown(t *testing.T) {
	srv := newModelServer(t, func(w http.ResponseWriter, req wireRequest, _ int) {
		if req.Model == fallbackModel {
			writeStatus(w, http.StatusTooManyRequests, "Rate limit error.")
			return
		}
		writeAnswer(w, "from-terra")
	})
	client := newFallbackClient(srv)
	client.primaryUnhealthyUntil.Store(time.Now().Add(time.Minute).UnixNano())

	resp, err, attempts := observedChat(t, client, ChatRequest{})
	if err != nil {
		t.Fatalf("Chat error = %v, want the primary's answer as the last resort", err)
	}
	if resp.Content != "from-terra" {
		t.Fatalf("Content = %q, want from-terra", resp.Content)
	}
	if got := srv.models(); !sameStrings(got, []string{fallbackModel, primaryModel}) {
		t.Fatalf("requests went to %v, want fallback then primary", got)
	}
	if len(attempts) != 2 || attempts[0].ErrorClass != OutboundErrorRateLimited || !attempts[0].Retried ||
		attempts[1].Call.Model != primaryModel || attempts[1].Outcome != OutboundAttemptSuccess {
		t.Fatalf("attempts = %#v", attempts)
	}
}

// A rejection of the request is not an upstream failure: the fallback would
// refuse the same request, so it is neither tried nor does the primary cool down.
func TestChatDoesNotRouteRequestRejectionsToFallback(t *testing.T) {
	srv := newModelServer(t, func(w http.ResponseWriter, req wireRequest, n int) {
		if n == 1 {
			writeStatus(w, http.StatusBadRequest, "context length exceeded")
			return
		}
		writeAnswer(w, "ok")
	})
	client := newFallbackClient(srv)

	_, err, attempts := observedChat(t, client, ChatRequest{})
	if err == nil {
		t.Fatal("Chat error = nil, want the 400 surfaced")
	}
	if len(attempts) != 1 || attempts[0].Retried || attempts[0].ErrorClass != OutboundErrorUpstream4xx {
		t.Fatalf("attempts = %#v, want one unretried upstream_4xx attempt", attempts)
	}
	if _, err, attempts := observedChat(t, client, ChatRequest{}); err != nil || attempts[0].Call.Model != primaryModel {
		t.Fatalf("a rejection must not open the cooldown: err = %v, attempts = %#v", err, attempts)
	}
	if got := srv.models(); !sameStrings(got, []string{primaryModel, primaryModel}) {
		t.Fatalf("requests went to %v, want the primary only", got)
	}
}

// The production failure that shows up as "[ModelError] LLM 上游错误" is an
// error event the gateway writes into a 200 stream, sometimes after content.
// It is classified from its OpenAI error type, routed to the fallback, and
// the primary's partial text never reaches the caller.
func TestStreamErrorEventRoutesToFallbackWithoutLeakingThePrefix(t *testing.T) {
	srv := newModelServer(t, func(w http.ResponseWriter, req wireRequest, _ int) {
		if req.Model == primaryModel {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"[trace_id: x] Rate limit error. This request would exceed accounts' rate limit. Please try again later.\",\"type\":\"rate_limit_error\",\"param\":null,\"code\":\"Too Many Requests\"}}\n\n"))
			return
		}
		writeAnswer(w, "whole")
	})
	client := newFallbackClient(srv)

	var deltas []string
	resp, err, attempts := observedChat(t, client, ChatRequest{OnTextDelta: func(delta string) { deltas = append(deltas, delta) }})
	if err != nil {
		t.Fatalf("Chat error = %v, want the fallback's answer", err)
	}
	if resp.Content != "whole" {
		t.Fatalf("Content = %q, want whole", resp.Content)
	}
	if !sameStrings(deltas, []string{"whole"}) {
		t.Fatalf("published deltas = %q, want only the fallback's; the failed primary stream must not leak its prefix", deltas)
	}
	if len(attempts) != 2 || attempts[0].Call.Model != primaryModel || attempts[0].ErrorClass != OutboundErrorRateLimited ||
		!attempts[0].Retried || attempts[0].ProviderFirstChunkMS == nil || attempts[1].Call.Model != fallbackModel {
		t.Fatalf("attempts = %#v", attempts)
	}
}

// An in-stream error event carries no HTTP status; its OpenAI error type is
// the only typed signal, and the request-describing types are the closed set
// that must not be retried or routed.
func TestStreamErrorEventStatusReadsTheOpenAIErrorType(t *testing.T) {
	for errorType, want := range map[string]int{
		"rate_limit_error":      http.StatusTooManyRequests,
		"invalid_request_error": http.StatusBadRequest,
		"authentication_error":  http.StatusBadRequest,
		"permission_error":      http.StatusBadRequest,
		"not_found_error":       http.StatusBadRequest,
		"insufficient_quota":    http.StatusBadRequest,
		"server_error":          http.StatusServiceUnavailable,
		"api_error":             http.StatusServiceUnavailable,
		"":                      http.StatusServiceUnavailable,
	} {
		if got := streamErrorEventStatus(errorType); got != want {
			t.Fatalf("streamErrorEventStatus(%q) = %d, want %d", errorType, got, want)
		}
	}
	event := func(errorType string) error { return &openai.APIError{Type: errorType, Message: "x"} }
	if got := traceOutboundErrorClass(context.Background(), event("rate_limit_error")); got != OutboundErrorRateLimited {
		t.Fatalf("in-stream rate_limit_error class = %q, want %q", got, OutboundErrorRateLimited)
	}
	if got := traceOutboundErrorClass(context.Background(), event("server_error")); got != OutboundErrorUpstream5xx {
		t.Fatalf("in-stream server_error class = %q, want %q", got, OutboundErrorUpstream5xx)
	}
	if got := traceOutboundErrorClass(context.Background(), event("invalid_request_error")); got != OutboundErrorUpstream4xx {
		t.Fatalf("in-stream invalid_request_error class = %q, want %q", got, OutboundErrorUpstream4xx)
	}
	if isTransientChatError(context.Background(), event("invalid_request_error")) {
		t.Fatal("an in-stream invalid_request_error must not be retried or routed")
	}
	if !isTransientChatError(context.Background(), event("server_error")) {
		t.Fatal("an in-stream server_error is an upstream failure and must be retried or routed")
	}
}

// Narrowing the request shape is learned about the endpoint, not about one
// attempt: once the primary rejected stream_options, the request routed to the
// fallback must not carry it again.
func TestRequestShapeLearnedOnPrimaryCarriesToFallback(t *testing.T) {
	srv := newModelServer(t, func(w http.ResponseWriter, req wireRequest, _ int) {
		if len(req.StreamOptions) != 0 {
			writeStatus(w, http.StatusBadRequest, "stream_options is not supported")
			return
		}
		if req.Model == primaryModel {
			writeStatus(w, http.StatusServiceUnavailable, "No available accounts.")
			return
		}
		writeAnswer(w, "from-luna")
	})
	client := newFallbackClient(srv)

	resp, err, attempts := observedChat(t, client, ChatRequest{})
	if err != nil {
		t.Fatalf("Chat error = %v", err)
	}
	if resp.Content != "from-luna" {
		t.Fatalf("Content = %q, want from-luna", resp.Content)
	}
	if got := srv.models(); !sameStrings(got, []string{primaryModel, primaryModel, fallbackModel}) {
		t.Fatalf("requests went to %v, want primary (with usage), primary (without), fallback (without)", got)
	}
	if len(attempts) != 3 || attempts[0].AttemptInCall != 1 || attempts[1].AttemptInCall != 2 || attempts[2].AttemptInCall != 3 ||
		!attempts[0].Retried || !attempts[1].Retried || attempts[2].Outcome != OutboundAttemptSuccess {
		t.Fatalf("attempts = %#v", attempts)
	}
}

// Without a fallback the second attempt stays on the only model, so a
// deployment that configures one model keeps today's retry exactly.
func TestWithoutFallbackTheRetryStaysOnThePrimary(t *testing.T) {
	srv := newModelServer(t, func(w http.ResponseWriter, _ wireRequest, n int) {
		if n == 1 {
			writeStatus(w, http.StatusServiceUnavailable, "No available accounts.")
			return
		}
		writeAnswer(w, "recovered")
	})
	client := NewClient(config.LLMConfig{BaseURL: srv.URL + "/v1", APIKey: "test-key", Model: primaryModel})

	resp, err, attempts := observedChat(t, client, ChatRequest{})
	if err != nil || resp.Content != "recovered" {
		t.Fatalf("Chat = (%v, %v), want recovered on the retry", resp, err)
	}
	if got := srv.models(); !sameStrings(got, []string{primaryModel, primaryModel}) {
		t.Fatalf("requests went to %v, want the primary twice", got)
	}
	if len(attempts) != 2 || attempts[0].Call.Model != primaryModel || attempts[1].Call.Model != primaryModel {
		t.Fatalf("attempts = %#v", attempts)
	}
}

func TestNewClientIgnoresFallbackEqualToPrimary(t *testing.T) {
	client := NewClient(config.LLMConfig{BaseURL: "http://127.0.0.1:1/v1", APIKey: "k", Model: primaryModel, FallbackModel: " " + primaryModel + " "})
	if client.fallbackModel != "" {
		t.Fatalf("fallbackModel = %q, want none when it names the primary", client.fallbackModel)
	}
	if got := client.modelOrder(time.Now()); !sameStrings(got, []string{primaryModel}) {
		t.Fatalf("modelOrder = %v, want the primary only", got)
	}
}
