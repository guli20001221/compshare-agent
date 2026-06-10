package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

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
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"retry ok\"}}]}\n\n"))
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

		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"retry recv ok\"}}]}\n\n"))
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
	if got := resp.Content; got != "retry recv ok" {
		t.Fatalf("Content = %q, want retry recv ok", got)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
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
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
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
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"{}\"}}]}\n\n"))
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
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
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
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"fallback ok\"}}]}\n\n"))
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
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"foo\",\"arguments\":\"{}\"}}]}}]}\n\n"))
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
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"bar\",\"arguments\":\"{}\"}}]}}]}\n\n"))
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
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"SearchKnowledge\",\"arguments\":\"{}\"}}]}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := NewClient(config.LLMConfig{BaseURL: srv.URL + "/v1", APIKey: "test-key", Model: "deepseek-v4-flash"})
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
