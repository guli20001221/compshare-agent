package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientEmbedSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/embeddings" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("missing or wrong auth header: %q", r.Header.Get("Authorization"))
		}
		var payload struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if payload.Model != "test-model" {
			t.Fatalf("wrong model: %q", payload.Model)
		}
		if len(payload.Input) != 1 || payload.Input[0] != "hello 你好" {
			t.Fatalf("wrong input: %#v", payload.Input)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []float32{0.1, -0.2, 0.3}}},
		})
	}))
	defer srv.Close()

	client, err := NewClient(ClientOptions{
		BaseURL: srv.URL,
		APIKey:  "test-key",
		Model:   "test-model",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	vec, err := client.Embed(context.Background(), "hello 你好")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 3 || vec[0] != 0.1 {
		t.Fatalf("unexpected vector: %#v", vec)
	}
}

func TestClientEmbedRetriesTransient(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []float32{1, 0, 0}}},
		})
	}))
	defer srv.Close()

	client, err := NewClient(ClientOptions{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	vec, err := client.Embed(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 3 || vec[0] != 1.0 {
		t.Fatalf("unexpected vector: %#v", vec)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 attempts, got %d", got)
	}
}

func TestClientEmbedRetriesOn308(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			http.Error(w, "moved", http.StatusPermanentRedirect)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []float32{0.5}}},
		})
	}))
	defer srv.Close()

	client, _ := NewClient(ClientOptions{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	vec, err := client.Embed(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if vec[0] != 0.5 {
		t.Fatalf("unexpected vector: %#v", vec)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 attempts, got %d", got)
	}
}

func TestClientEmbedReturnsErrorOnPermanentFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	client, _ := NewClient(ClientOptions{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	_, err := client.Embed(context.Background(), "hi")
	if err == nil {
		t.Fatal("expected error on HTTP 400")
	}
	if !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("expected HTTP 400 in error, got: %v", err)
	}
}

func TestClientTimeoutTriggersError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		http.Error(w, "ok", http.StatusOK)
	}))
	defer srv.Close()

	client, _ := NewClient(ClientOptions{
		BaseURL: srv.URL,
		APIKey:  "k",
		Model:   "m",
		Timeout: 30 * time.Millisecond,
	})
	_, err := client.Embed(context.Background(), "hi")
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestNewClientValidatesOptions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts ClientOptions
	}{
		{name: "missing key", opts: ClientOptions{BaseURL: "x", Model: "m"}},
		{name: "missing url", opts: ClientOptions{APIKey: "k", Model: "m"}},
		{name: "missing model", opts: ClientOptions{BaseURL: "x", APIKey: "k"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewClient(tc.opts); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestEmbedderInterfaceImpliesClient(t *testing.T) {
	t.Parallel()
	var _ Embedder = (*Client)(nil)
}

// Ensure transientError unwraps to its underlying cause so caller-side
// errors.As(*transientError) introspection works against wrapped HTTP errors.
func TestTransientErrorUnwrap(t *testing.T) {
	t.Parallel()
	inner := errors.New("boom")
	te := &transientError{err: inner}
	if !errors.Is(te, inner) {
		t.Fatal("transientError must wrap its inner err")
	}
}
