package reranker

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

func TestClientRerankSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/rerank" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("missing or wrong auth header: %q", r.Header.Get("Authorization"))
		}
		var payload struct {
			Model     string   `json:"model"`
			Query     string   `json:"query"`
			Documents []string `json:"documents"`
			TopN      int      `json:"top_n,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if payload.Model != "qwen3-reranker-8b" || payload.Query != "怎么查余额" || len(payload.Documents) != 3 || payload.TopN != 2 {
			t.Fatalf("unexpected request payload: %+v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"index": 0, "relevance_score": 0.9036},
				{"index": 2, "relevance_score": 0.00012},
			},
		})
	}))
	defer srv.Close()
	client, err := NewModelverseClient(ClientOptions{
		BaseURL: srv.URL,
		APIKey:  "test-key",
		Model:   "qwen3-reranker-8b",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	got, err := client.Rerank(context.Background(), "怎么查余额", []string{"a", "b", "c"}, 2)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 results, got %d", len(got))
	}
	if got[0].Index != 0 || got[0].Score < 0.9 {
		t.Fatalf("top result wrong: %+v", got[0])
	}
	if got[1].Index != 2 {
		t.Fatalf("second result wrong: %+v", got[1])
	}
}

// Defensive sort: even if the server returns results out of order, the
// client returns them desc-sorted so the retriever's downstream code can
// trust the ranking without re-sorting.
func TestClientRerankSortsDescIfServerMisOrders(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"index": 1, "relevance_score": 0.1},
				{"index": 0, "relevance_score": 0.9},
				{"index": 2, "relevance_score": 0.5},
			},
		})
	}))
	defer srv.Close()
	client, _ := NewModelverseClient(ClientOptions{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	got, err := client.Rerank(context.Background(), "q", []string{"a", "b", "c"}, 0)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(got) != 3 || got[0].Index != 0 || got[1].Index != 2 || got[2].Index != 1 {
		t.Fatalf("not desc-sorted: %+v", got)
	}
}

func TestClientRerankRetriesTransient(t *testing.T) {
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
			"results": []map[string]any{{"index": 0, "relevance_score": 0.5}},
		})
	}))
	defer srv.Close()
	client, _ := NewModelverseClient(ClientOptions{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	got, err := client.Rerank(context.Background(), "q", []string{"a"}, 0)
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(got) != 1 || got[0].Index != 0 {
		t.Fatalf("unexpected result: %+v", got)
	}
	if c := calls.Load(); c != 2 {
		t.Fatalf("expected 2 attempts, got %d", c)
	}
}

func TestClientRerankErrorOnPermanentFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()
	client, _ := NewModelverseClient(ClientOptions{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	_, err := client.Rerank(context.Background(), "q", []string{"a"}, 0)
	if err == nil {
		t.Fatal("want error on 400")
	}
	if !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("want HTTP 400 in error, got: %v", err)
	}
}

func TestClientRerankTimeoutTriggersError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		http.Error(w, "ok", http.StatusOK)
	}))
	defer srv.Close()
	client, _ := NewModelverseClient(ClientOptions{
		BaseURL: srv.URL,
		APIKey:  "k",
		Model:   "m",
		Timeout: 30 * time.Millisecond,
	})
	_, err := client.Rerank(context.Background(), "q", []string{"a"}, 0)
	if err == nil {
		t.Fatal("want timeout error")
	}
}

func TestClientRerankEmptyDocsReturnsNil(t *testing.T) {
	t.Parallel()
	// No server should be hit; reachable URL is fine.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called when docs is empty")
	}))
	defer srv.Close()
	client, _ := NewModelverseClient(ClientOptions{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	got, err := client.Rerank(context.Background(), "q", nil, 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("want (nil, nil), got (%v, %v)", got, err)
	}
}

func TestClientRerankEmptyResultsIsError(t *testing.T) {
	// Empty Results from server (e.g. malformed response) is a permanent
	// error so caller can fall back to the prior stage's top-K.
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{}})
	}))
	defer srv.Close()
	client, _ := NewModelverseClient(ClientOptions{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	_, err := client.Rerank(context.Background(), "q", []string{"a"}, 0)
	if err == nil || !strings.Contains(err.Error(), "empty results") {
		t.Fatalf("want empty-results error, got: %v", err)
	}
}

func TestNewModelverseClientValidatesOptions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts ClientOptions
	}{
		{"missing key", ClientOptions{BaseURL: "x", Model: "m"}},
		{"missing url", ClientOptions{APIKey: "k", Model: "m"}},
		{"missing model", ClientOptions{BaseURL: "x", APIKey: "k"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewModelverseClient(tc.opts); err == nil {
				t.Fatal("want validation error")
			}
		})
	}
}

func TestTransientErrorUnwrap(t *testing.T) {
	t.Parallel()
	inner := errors.New("boom")
	te := &transientError{err: inner}
	if !errors.Is(te, inner) {
		t.Fatal("transientError must wrap its inner err")
	}
}
