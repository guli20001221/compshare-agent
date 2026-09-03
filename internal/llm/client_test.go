package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	openai "github.com/sashabaranov/go-openai"
)

func TestClientChatRetriesTransientStreamOpenError(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		attempt := atomic.AddInt32(&attempts, 1)
		if attempt == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("response writer does not support hijacking")
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Fatalf("hijack: %v", err)
			}
			_ = conn.Close()
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"retry ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := NewClient(config.LLMConfig{
		BaseURL: srv.URL + "/v1",
		APIKey:  "test-key",
		Model:   "test-model",
	})

	resp, err := client.Chat(context.Background(), ChatRequest{
		Messages: []openai.ChatCompletionMessage{{
			Role:    openai.ChatMessageRoleUser,
			Content: "hello",
		}},
	})
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if got := resp.Content; got != "retry ok" {
		t.Fatalf("Content = %q, want retry ok", got)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

func TestChatHTTPClientLeavesStreamLifetimeToCallerContext(t *testing.T) {
	for _, baseURL := range []string{"https://api.example.test/v1", "http://127.0.0.1:8080/v1"} {
		client := chatHTTPClient(baseURL)
		if client.Timeout != 0 {
			t.Fatalf("%s total timeout = %s, want 0 so the caller context owns stream lifetime", baseURL, client.Timeout)
		}
	}
	localTransport, ok := chatHTTPClient("http://localhost:8080/v1").Transport.(*http.Transport)
	if !ok || localTransport.Proxy != nil {
		t.Fatal("localhost client must keep the explicit no-proxy transport")
	}
}

func TestChatHTTPClientBypassesProxyOnlyForLoopbackEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		baseURL string
		direct  bool
	}{
		{name: "IPv4 loopback", baseURL: "http://127.0.0.1:8000/v1", direct: true},
		{name: "IPv6 loopback", baseURL: "http://[::1]:8000/v1", direct: true},
		{name: "localhost", baseURL: "http://LOCALHOST:8000/v1", direct: true},
		{name: "remote host containing loopback text", baseURL: "https://127.0.0.1.example.com/v1"},
		{name: "ordinary remote upstream", baseURL: "https://api.example.com/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := chatHTTPClient(tc.baseURL)
			_, direct := client.Transport.(*http.Transport)
			if direct != tc.direct {
				t.Fatalf("direct transport = %v, want %v", direct, tc.direct)
			}
		})
	}
}

func TestClientChatOutboundObserverCountsEveryActualAttempt(t *testing.T) {
	var attempts int32
	tools := []openai.Tool{{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "lookup",
			Description: "look up one item",
			Parameters:  map[string]any{"type": "object"},
		},
	}}
	wantToolJSON, err := json.Marshal(tools)
	if err != nil {
		t.Fatalf("marshal expected tools: %v", err)
	}
	var sentToolJSON string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := atomic.AddInt32(&attempts, 1)
		if attempt == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("response writer does not support hijacking")
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Fatalf("hijack: %v", err)
			}
			_ = conn.Close()
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read retry request: %v", err)
		}
		var wireRequest struct {
			Tools json.RawMessage `json:"tools"`
		}
		if err := json.Unmarshal(body, &wireRequest); err != nil {
			t.Fatalf("decode retry request: %v", err)
		}
		sentToolJSON = string(wireRequest.Tools)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := NewClient(config.LLMConfig{BaseURL: srv.URL + "/v1", APIKey: "test-key", Model: "test-model"})
	var observed []OutboundCall
	ctx := WithOutboundCallObserver(context.Background(), func(call OutboundCall) {
		observed = append(observed, call)
	})
	var completed []OutboundCallResult
	ctx = WithOutboundCallResultObserver(ctx, func(result OutboundCallResult) {
		completed = append(completed, result)
	})
	wantToolCount, wantToolRunes, wantToolHash := observeToolWindow(tools)

	resp, err := client.Chat(ctx, ChatRequest{Messages: []openai.ChatCompletionMessage{{
		Role: openai.ChatMessageRoleUser, Content: "hello",
	}}, Tools: tools})
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("Content = %q, want ok", resp.Content)
	}
	if sentToolJSON != string(wantToolJSON) {
		t.Fatalf("SDK sent tools = %s, observed boundary serialized %s", sentToolJSON, wantToolJSON)
	}
	if got := len(observed); got != 2 {
		t.Fatalf("observer calls = %d, want 2 actual attempts", got)
	}
	for i, call := range observed {
		if call.Model != "test-model" {
			t.Fatalf("observer call %d model = %q, want test-model", i, call.Model)
		}
		if call.Provider != ProviderOpenAICompatible {
			t.Fatalf("observer call %d provider = %q, want %s", i, call.Provider, ProviderOpenAICompatible)
		}
	}
	if got := len(completed); got != 2 {
		t.Fatalf("completed observer calls = %d, want both actual attempts", got)
	}
	if completed[0].AttemptInCall != 1 || completed[0].Outcome != OutboundAttemptError ||
		!completed[0].Retried || completed[0].ErrorClass == "" ||
		completed[0].StopReason != "" || completed[0].ProviderFirstChunkMS != nil {
		t.Fatalf("failed attempt trace = %#v", completed[0])
	}
	if completed[1].AttemptInCall != 2 || completed[1].Outcome != OutboundAttemptSuccess ||
		completed[1].Retried || completed[1].StopReason != "stop" || completed[1].ProviderFirstChunkMS == nil {
		t.Fatalf("successful attempt trace = %#v", completed[1])
	}
	for i, attempt := range completed {
		if attempt.ToolCount != wantToolCount || attempt.ToolWindowRunes != wantToolRunes || attempt.ToolWindowHash != wantToolHash {
			t.Fatalf("attempt %d tool observation = (%d, %d, %q), want (%d, %d, %q)",
				i, attempt.ToolCount, attempt.ToolWindowRunes, attempt.ToolWindowHash,
				wantToolCount, wantToolRunes, wantToolHash)
		}
	}
}

func TestClientChatAttemptUsagePreservesMissingZeroAndPositiveCacheStates(t *testing.T) {
	zero := 0
	prompt := 120
	cached := 80
	tests := []struct {
		name       string
		usageJSON  string
		wantPrompt *int
		wantCached *int
	}{
		{name: "no usage block"},
		{name: "empty usage object", usageJSON: `,"usage":{}`},
		{name: "usage without prompt details", usageJSON: `,"usage":{"prompt_tokens":0,"completion_tokens":1,"total_tokens":1}`, wantPrompt: &zero},
		{name: "details report zero cache hit", usageJSON: `,"usage":{"prompt_tokens":120,"completion_tokens":1,"total_tokens":121,"prompt_tokens_details":{"cached_tokens":0}}`, wantPrompt: &prompt, wantCached: &zero},
		{name: "details report positive cache hit", usageJSON: `,"usage":{"prompt_tokens":120,"completion_tokens":1,"total_tokens":121,"prompt_tokens_details":{"cached_tokens":80}}`, wantPrompt: &prompt, wantCached: &cached},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]` + tc.usageJSON + "}\n\n"))
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
			}))
			defer srv.Close()

			client := NewClient(config.LLMConfig{BaseURL: srv.URL + "/v1", APIKey: "test-key", Model: "test-model"})
			var attempts []OutboundCallResult
			ctx := WithOutboundCallResultObserver(context.Background(), func(result OutboundCallResult) {
				attempts = append(attempts, result)
			})
			_, err := client.Chat(ctx, ChatRequest{Messages: []openai.ChatCompletionMessage{{
				Role: openai.ChatMessageRoleUser, Content: "hello",
			}}})
			if err != nil {
				t.Fatalf("Chat error: %v", err)
			}
			if len(attempts) != 1 {
				t.Fatalf("attempts = %d, want 1", len(attempts))
			}
			assertOptionalInt(t, "prompt tokens", attempts[0].PromptTokens, tc.wantPrompt)
			assertOptionalInt(t, "cached prompt tokens", attempts[0].CachedPromptTokens, tc.wantCached)
			if attempts[0].ToolCount != 0 || attempts[0].ToolWindowRunes != 0 || attempts[0].ToolWindowHash != "" {
				t.Fatalf("tool-free attempt unexpectedly has tool observation: %#v", attempts[0])
			}
		})
	}
}

func TestObserveToolWindowMeasuresOrderedWireJSON(t *testing.T) {
	first := openai.Tool{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{
		Name: "first", Description: "第一个", Parameters: map[string]any{"type": "object"},
	}}
	second := openai.Tool{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{
		Name: "second", Description: "second", Parameters: map[string]any{"type": "object"},
	}}
	tools := []openai.Tool{first, second}
	raw, err := json.Marshal(tools)
	if err != nil {
		t.Fatalf("marshal expected tools: %v", err)
	}
	sum := sha256.Sum256(raw)
	wantHash := "sha256:" + hex.EncodeToString(sum[:])

	count, runes, hash := observeToolWindow(tools)
	if count != 2 || runes != len([]rune(string(raw))) || hash != wantHash {
		t.Fatalf("observeToolWindow = (%d, %d, %q), want (2, %d, %q)",
			count, runes, hash, len([]rune(string(raw))), wantHash)
	}
	_, _, reversedHash := observeToolWindow([]openai.Tool{second, first})
	if reversedHash == hash {
		t.Fatal("reordering tools must change the order-sensitive window hash")
	}
	if count, runes, hash := observeToolWindow(nil); count != 0 || runes != 0 || hash != "" {
		t.Fatalf("nil tools = (%d, %d, %q), want zero observation", count, runes, hash)
	}
}

func assertOptionalInt(t *testing.T, label string, got, want *int) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Fatalf("%s = %d, want nil", label, *got)
		}
		return
	}
	if got == nil || *got != *want {
		if got == nil {
			t.Fatalf("%s = nil, want %d", label, *want)
		}
		t.Fatalf("%s = %d, want %d", label, *got, *want)
	}
}

func TestTraceOutboundErrorClassUsesTypedSignalsNotErrorText(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want string
	}{
		{name: "cancelled", ctx: context.Background(), err: context.Canceled, want: OutboundErrorCancelled},
		{name: "deadline", ctx: context.Background(), err: context.DeadlineExceeded, want: OutboundErrorDeadline},
		{name: "rate limit", ctx: context.Background(), err: &openai.APIError{HTTPStatusCode: 429}, want: OutboundErrorRateLimited},
		{name: "upstream 4xx", ctx: context.Background(), err: &openai.APIError{HTTPStatusCode: 400}, want: OutboundErrorUpstream4xx},
		{name: "upstream 5xx", ctx: context.Background(), err: &openai.RequestError{HTTPStatusCode: 503}, want: OutboundErrorUpstream5xx},
		{name: "truncated stream", ctx: context.Background(), err: io.ErrUnexpectedEOF, want: OutboundErrorStream},
		{name: "text that looks like status stays other", ctx: context.Background(), err: errors.New("429 password=hunter2"), want: OutboundErrorOther},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := traceOutboundErrorClass(tc.ctx, tc.err); got != tc.want {
				t.Fatalf("traceOutboundErrorClass() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCancelledOverloadBackoffDoesNotClaimASecondAttempt(t *testing.T) {
	var outbound int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&outbound, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"overloaded","type":"server_error"}}`))
	}))
	defer srv.Close()

	client := NewClient(config.LLMConfig{BaseURL: srv.URL + "/v1", APIKey: "test-key", Model: "test-model"})
	// Leave ample time for the first httptest request even under -race, while
	// staying well below the client's 900 ms overload backoff.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var attempts []OutboundCallResult
	ctx = WithOutboundCallResultObserver(ctx, func(result OutboundCallResult) {
		attempts = append(attempts, result)
	})

	_, err := client.Chat(ctx, ChatRequest{Messages: []openai.ChatCompletionMessage{{
		Role: openai.ChatMessageRoleUser, Content: "hello",
	}}})
	if err == nil {
		t.Fatal("expected the overloaded request to fail")
	}
	if got := atomic.LoadInt32(&outbound); got != 1 {
		t.Fatalf("actual outbound attempts = %d, want 1", got)
	}
	if len(attempts) != 1 || attempts[0].Retried || attempts[0].ErrorClass != OutboundErrorUpstream5xx {
		t.Fatalf("attempt trace must report no actual retry: %#v", attempts)
	}
}

func TestClientChatRetriesTransientStreamRecvError(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		if attempt == 1 {
			w.Header().Set("Content-Length", "1024")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
			return
		}

		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"retry recv ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := NewClient(config.LLMConfig{
		BaseURL: srv.URL + "/v1",
		APIKey:  "test-key",
		Model:   "test-model",
	})

	var delivered []string
	resp, err := client.Chat(context.Background(), ChatRequest{
		Messages: []openai.ChatCompletionMessage{{
			Role:    openai.ChatMessageRoleUser,
			Content: "hello",
		}},
		OnTextDelta: func(delta string) { delivered = append(delivered, delta) },
	})
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if got := resp.Content; got != "retry recv ok" {
		t.Fatalf("Content = %q, want retry recv ok", got)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
	if got := strings.Join(delivered, ""); got != "retry recv ok" {
		t.Fatalf("delivered deltas = %q, want only the successful retry", got)
	}
}

func TestClientChatCarriesLengthFinishReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"incomplete\"},\"finish_reason\":\"length\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := NewClient(config.LLMConfig{BaseURL: srv.URL + "/v1", APIKey: "test-key", Model: "test-model"})
	resp, err := client.Chat(context.Background(), ChatRequest{Messages: []openai.ChatCompletionMessage{{
		Role: openai.ChatMessageRoleUser, Content: "hello",
	}}})
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if got := resp.StopReason; got != "length" {
		t.Fatalf("StopReason = %q, want length", got)
	}
	if !resp.OutputIncomplete() {
		t.Fatal("length finish_reason must be treated as truncated output")
	}
}

func TestClientChatRejectsStreamWithoutTerminalFinishReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "partial_text_eof", body: "data: {\"choices\":[{\"delta\":{\"content\":\"安装 GPU 版（以\"}}]}\n\n"},
		{name: "partial_text_done_without_finish", body: "data: {\"choices\":[{\"delta\":{\"content\":\"安装 GPU 版（以\"}}]}\n\ndata: [DONE]\n\n"},
		{name: "partial_tool_eof", body: "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"partial\",\"type\":\"function\",\"function\":{\"name\":\"SearchKnowledge\",\"arguments\":\"{}\"}}]}}]}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&attempts, 1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.body)
				// A normal HTTP body EOF after a valid delta is not a completed
				// model choice, even though the SDK reports it as io.EOF.
			}))
			defer srv.Close()
			client := NewClient(config.LLMConfig{BaseURL: srv.URL + "/v1", APIKey: "test-key", Model: "test-model"})
			var completed []OutboundCallResult
			ctx := WithOutboundCallResultObserver(context.Background(), func(result OutboundCallResult) {
				completed = append(completed, result)
			})
			var delivered []string
			resp, err := client.Chat(ctx, ChatRequest{
				Messages:    []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hello"}},
				OnTextDelta: func(delta string) { delivered = append(delivered, delta) },
			})
			if !errors.Is(err, io.ErrUnexpectedEOF) || resp != nil {
				t.Fatalf("response=%#v error=%v, want no response and unexpected EOF", resp, err)
			}
			if len(delivered) != 0 {
				t.Fatalf("partial attempt leaked to caller: %q", delivered)
			}
			if got := atomic.LoadInt32(&attempts); got != maxChatAttempts || len(completed) != maxChatAttempts {
				t.Fatalf("attempts=%d observations=%d, want bounded retry count %d", got, len(completed), maxChatAttempts)
			}
			for i, attempt := range completed {
				if attempt.Outcome != OutboundAttemptError || attempt.ErrorClass != OutboundErrorStream || attempt.StopReason != "" || attempt.Retried != (i+1 < maxChatAttempts) {
					t.Fatalf("incomplete attempt recorded as successful: %#v", attempt)
				}
			}
		})
	}
}

func TestChatResponseFailsClosedOnUnknownNonCompleteFinishReason(t *testing.T) {
	if (ChatResponse{StopReason: "content_filter"}).OutputIncomplete() != true {
		t.Fatal("content-filtered output must not be accepted as a complete answer")
	}
	if (ChatResponse{StopReason: "tool_calls"}).OutputIncomplete() {
		t.Fatal("a completed tool-calls response is a valid ReAct step")
	}
}

func TestTraceFinishReasonUsesClosedSet(t *testing.T) {
	if got := TraceFinishReason(""); got != "unspecified" {
		t.Fatalf("TraceFinishReason(empty) = %q, want unspecified", got)
	}
	if got := TraceFinishReason("null"); got != "other" {
		t.Fatalf("TraceFinishReason(literal null) = %q, want other", got)
	}
	if got := TraceFinishReason("TOOL_CALLS"); got != "tool_calls" {
		t.Fatalf("TraceFinishReason(tool_calls) = %q, want tool_calls", got)
	}
	if got := TraceFinishReason("provider diagnostic: token cap 123"); got != "other" {
		t.Fatalf("TraceFinishReason(unknown) = %q, want other", got)
	}
}

func TestClientChatDoesNotRetryProviderStatusError(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"tool_choice unsupported","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()

	client := NewClient(config.LLMConfig{
		BaseURL: srv.URL + "/v1",
		APIKey:  "test-key",
		Model:   "test-model",
	})

	_, err := client.Chat(context.Background(), ChatRequest{
		Messages: []openai.ChatCompletionMessage{{
			Role:    openai.ChatMessageRoleUser,
			Content: "hello",
		}},
	})
	if err == nil {
		t.Fatal("Chat error = nil, want provider error")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
}

func TestClientChatCapturesStreamingUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("parse request: %v", err)
		}
		streamOptions, ok := req["stream_options"].(map[string]any)
		if !ok || streamOptions["include_usage"] != true {
			t.Fatalf("stream_options = %#v, want include_usage=true", req["stream_options"])
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := NewClient(config.LLMConfig{
		BaseURL: srv.URL + "/v1",
		APIKey:  "test-key",
		Model:   "test-model",
	})

	resp, err := client.Chat(context.Background(), ChatRequest{
		Messages: []openai.ChatCompletionMessage{{
			Role:    openai.ChatMessageRoleUser,
			Content: "hello",
		}},
	})
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if resp.Content != "ok" || resp.Usage.TotalTokens != 18 ||
		resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 7 {
		t.Fatalf("response = %#v", resp)
	}
}

func TestClientChatSendsResponseFormatWhenRequested(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("parse request: %v", err)
		}
		responseFormat, ok := req["response_format"].(map[string]any)
		if !ok {
			t.Fatalf("response_format = %#v, want object", req["response_format"])
		}
		if responseFormat["type"] != "json_object" {
			t.Fatalf("response_format.type = %#v, want json_object", responseFormat["type"])
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"{}\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := NewClient(config.LLMConfig{
		BaseURL: srv.URL + "/v1",
		APIKey:  "test-key",
		Model:   "test-model",
	})

	_, err := client.Chat(context.Background(), ChatRequest{
		Messages: []openai.ChatCompletionMessage{{
			Role:    openai.ChatMessageRoleUser,
			Content: "return json",
		}},
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONObject,
		},
	})
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}
}

func TestClientChatOmitsResponseFormatForToolRequestsByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("parse request: %v", err)
		}
		if _, ok := req["response_format"]; ok {
			t.Fatalf("response_format should be omitted for ordinary tool request: %s", string(body))
		}
		if _, ok := req["tools"]; !ok {
			t.Fatalf("tools should still be sent: %s", string(body))
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := NewClient(config.LLMConfig{
		BaseURL: srv.URL + "/v1",
		APIKey:  "test-key",
		Model:   "test-model",
	})

	_, err := client.Chat(context.Background(), ChatRequest{
		Messages: []openai.ChatCompletionMessage{{
			Role:    openai.ChatMessageRoleUser,
			Content: "call tool if needed",
		}},
		Tools: []openai.Tool{{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "DescribeCompShareInstance",
				Description: "describe instance",
				Parameters:  map[string]any{"type": "object"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}
}

func TestClientChatFallsBackWhenStreamingUsageUnsupported(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := atomic.AddInt32(&attempts, 1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if attempt == 1 {
			if !strings.Contains(string(body), "stream_options") {
				t.Fatalf("first request should ask for usage: %s", string(body))
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid param: stream_options include_usage not support","type":"invalid_request_error"}}`))
			return
		}
		if strings.Contains(string(body), "stream_options") {
			t.Fatalf("fallback request should omit stream_options: %s", string(body))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"fallback ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := NewClient(config.LLMConfig{
		BaseURL: srv.URL + "/v1",
		APIKey:  "test-key",
		Model:   "test-model",
	})

	resp, err := client.Chat(context.Background(), ChatRequest{
		Messages: []openai.ChatCompletionMessage{{
			Role:    openai.ChatMessageRoleUser,
			Content: "hello",
		}},
	})
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
	if resp.Content != "fallback ok" || resp.Usage.TotalTokens != 0 {
		t.Fatalf("response = %#v", resp)
	}
}

func TestUsageUnsupportedFallbackRequiresExplicitUnsupportedSignal(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "not support",
			err:  errors.New("Invalid param: stream_options include_usage not support"),
			want: true,
		},
		{
			name: "unknown parameter",
			err:  errors.New("unknown parameter: stream_options.include_usage"),
			want: true,
		},
		{
			name: "invalid but not unsupported",
			err:  errors.New("Invalid param: stream_options include_usage must be a boolean"),
			want: false,
		},
		{
			name: "unrelated unsupported",
			err:  errors.New("tool_choice unsupported"),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUsageUnsupportedChatError(tc.err); got != tc.want {
				t.Fatalf("isUsageUnsupportedChatError(%q) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsTransientChatErrorClassifiesRetryableMessages(t *testing.T) {
	ctx := context.Background()
	for _, msg := range []string{
		"llm stream recv: unexpected EOF",
		"llm stream: connection reset by peer",
		"llm stream: TLS handshake timeout",
		"llm stream: timeout awaiting response headers",
	} {
		if !isTransientChatError(ctx, errors.New(msg)) {
			t.Fatalf("isTransientChatError(%q) = false, want true", msg)
		}
	}
}

func TestIsTransientChatErrorDoesNotRetryContextErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if isTransientChatError(ctx, context.Canceled) {
		t.Fatal("context.Canceled classified as transient")
	}
	if isTransientChatError(context.Background(), context.DeadlineExceeded) {
		t.Fatal("context deadline exceeded classified as transient")
	}
}

func TestOnTextDeltaCalledInOrderForNonEmptyDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// three text deltas + one tool-call-only chunk (no content)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"foo\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := NewClient(config.LLMConfig{
		BaseURL: srv.URL + "/v1",
		APIKey:  "test-key",
		Model:   "test-model",
	})

	var got []string
	resp, err := client.Chat(context.Background(), ChatRequest{
		Messages:    []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}},
		OnTextDelta: func(s string) { got = append(got, s) },
	})
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	// Delta callback must be called only for non-empty content chunks
	if len(got) != 2 || got[0] != "你" || got[1] != "好" {
		t.Fatalf("OnTextDelta calls = %v, want [\"你\", \"好\"]", got)
	}
	// Final assembled content must include both characters
	if resp.Content != "你好" {
		t.Fatalf("Content = %q, want \"你好\"", resp.Content)
	}
}

func TestOnTextDeltaNotCalledForToolCallOnlyChunks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// only a tool-call chunk, no text content
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"bar\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := NewClient(config.LLMConfig{
		BaseURL: srv.URL + "/v1",
		APIKey:  "test-key",
		Model:   "test-model",
	})

	called := false
	_, err := client.Chat(context.Background(), ChatRequest{
		Messages:    []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "use tool"}},
		OnTextDelta: func(s string) { called = true },
	})
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if called {
		t.Fatal("OnTextDelta should not be called for tool-call-only chunks")
	}
}

// TestClientChatFallsBackToAutoWhenForcedToolChoiceUnsupported pins the P0 fix:
// when the upstream rejects a forced tool_choice in thinking mode (per-key
// Modelverse behavior), Chat retries once with auto rather than failing the turn.
// The retry must drop tool_choice; the engine-injected note keeps the tool likely.
func TestClientChatFallsBackToAutoWhenForcedToolChoiceUnsupported(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := atomic.AddInt32(&attempts, 1)
		body, _ := io.ReadAll(r.Body)
		if attempt == 1 {
			if !strings.Contains(string(body), "tool_choice") {
				t.Fatalf("first request should carry forced tool_choice: %s", string(body))
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"The tool_choice parameter does not support being set to required or object in thinking mode","type":"invalid_request_error"}}`))
			return
		}
		if strings.Contains(string(body), `"tool_choice"`) {
			t.Fatalf("fallback request must omit tool_choice (auto): %s", string(body))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"SearchKnowledge\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := NewClient(config.LLMConfig{BaseURL: srv.URL + "/v1", APIKey: "test-key", Model: "gpt-5.6-terra"})
	resp, err := client.Chat(context.Background(), ChatRequest{
		Messages:   []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}},
		Tools:      []openai.Tool{{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{Name: "SearchKnowledge", Parameters: map[string]any{"type": "object"}}}},
		ToolChoice: openai.ToolChoice{Type: openai.ToolTypeFunction, Function: openai.ToolFunction{Name: "SearchKnowledge"}},
	})
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("attempts = %d, want 2 (forced 400, then auto retry)", got)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Function.Name != "SearchKnowledge" {
		t.Fatalf("expected SearchKnowledge tool call after auto fallback, got %#v", resp.ToolCalls)
	}
	// The degrade must be observable: the response came from an UNFORCED retry,
	// so ForcedToolChoiceDegraded lets a caller that required the forcing fall
	// back instead of trusting it.
	if !resp.ForcedToolChoiceDegraded {
		t.Fatal("ForcedToolChoiceDegraded must be true after the auto fallback fired")
	}
}

// TestClientChatForcedToolChoiceHonoredIsNotDegraded pins the negative: when the
// provider honors the forced tool_choice on the first try, the response must NOT
// be flagged degraded (only the silent auto retry sets it).
func TestClientChatForcedToolChoiceHonoredIsNotDegraded(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"SearchKnowledge\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := NewClient(config.LLMConfig{BaseURL: srv.URL + "/v1", APIKey: "test-key", Model: "gpt-5.6-terra"})
	resp, err := client.Chat(context.Background(), ChatRequest{
		Messages:   []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}},
		Tools:      []openai.Tool{{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{Name: "SearchKnowledge", Parameters: map[string]any{"type": "object"}}}},
		ToolChoice: "required",
	})
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("attempts = %d, want 1 (forced honored, no retry)", got)
	}
	if resp.ForcedToolChoiceDegraded {
		t.Fatal("ForcedToolChoiceDegraded must be false when the forced choice was honored")
	}
}

func TestForcedToolChoiceClassifiers(t *testing.T) {
	// Only the thinking-mode message triggers the auto fallback.
	if !isForcedToolChoiceUnsupportedError(errors.New("llm stream: status 400 The tool_choice parameter does not support being set to required or object in thinking mode")) {
		t.Error("thinking-mode tool_choice 400 should match")
	}
	for _, e := range []error{
		errors.New("tool_choice unsupported"), // mirrors TestClientChatDoesNotRetryProviderStatusError
		errors.New("no function named 'SearchKnowledge' was specified in the 'tools' parameter"),
		errors.New("some other 400"),
		nil,
	} {
		if isForcedToolChoiceUnsupportedError(e) {
			t.Errorf("non-thinking-mode error must not match: %v", e)
		}
	}
	// Object struct and "required" are forced; nil/auto/none are not.
	if !isForcedToolChoice(openai.ToolChoice{Type: openai.ToolTypeFunction, Function: openai.ToolFunction{Name: "X"}}) {
		t.Error("object tool_choice should be forced")
	}
	if !isForcedToolChoice("required") {
		t.Error(`"required" should be forced`)
	}
	for _, tc := range []any{nil, "auto", "none"} {
		if isForcedToolChoice(tc) {
			t.Errorf("%v should not be forced", tc)
		}
	}
}
