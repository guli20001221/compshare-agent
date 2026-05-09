package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
