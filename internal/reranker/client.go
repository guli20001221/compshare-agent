// Package reranker wraps the ModelVerse /v1/rerank endpoint (cohere-style
// schema) so the hybrid retriever can add a cross-encoder rerank stage on
// top of cosine top-K.
//
// Lane B.0 API probe (2026-05-19) confirmed the endpoint accepts:
//   POST /v1/rerank {"model","query","documents":[...],"top_n":N}
// and returns results sorted by descending relevance_score:
//   {"results":[{"index":<orig-pos>,"relevance_score":<float>}, ...]}
//
// The retriever depends only on the Client interface so unit tests can
// inject deterministic results without an HTTP server (mirrors the
// VectorEmbedder pattern in internal/knowledge/retriever.go).
package reranker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Result holds a single reranker output: the original-document index plus
// the model's relevance score. Callers look up the original doc by Index;
// the server already returns Results sorted by Score desc.
type Result struct {
	Index int
	Score float64
}

// Client returns reranker scores for (query, docs). On transient failures
// (timeout, 429, 5xx, 308) implementations may retry; on permanent
// failures or empty responses they surface an error so the caller (the
// retriever's reranker stage) can fall back to the prior stage's top-K.
type Client interface {
	Rerank(ctx context.Context, query string, docs []string, topN int) ([]Result, error)
}

// ClientOptions wires a ModelVerse-style /v1/rerank client. Mirrors
// internal/embedding.ClientOptions intentionally so cmd/trace.go can read
// both from the same env block.
type ClientOptions struct {
	BaseURL    string
	APIKey     string
	Model      string
	HTTPClient *http.Client
	Timeout    time.Duration
}

type modelverseClient struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
	model      string
}

// NewModelverseClient validates configuration and returns a ready-to-use
// Client backed by ModelVerse /v1/rerank.
func NewModelverseClient(opts ClientOptions) (Client, error) {
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, errors.New("reranker client: APIKey is required")
	}
	if strings.TrimSpace(opts.BaseURL) == "" {
		return nil, errors.New("reranker client: BaseURL is required")
	}
	if strings.TrimSpace(opts.Model) == "" {
		return nil, errors.New("reranker client: Model is required")
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		timeout := opts.Timeout
		if timeout <= 0 {
			// B.0 probe measured ~3.8s for a 50-doc single batch. Default to
			// 5s — bounds tail while covering our top-20 use case
			// comfortably. Override via ClientOptions.Timeout / env
			// RAG_RERANKER_TIMEOUT_MS.
			timeout = 5 * time.Second
		}
		httpClient = &http.Client{Timeout: timeout}
	}
	return &modelverseClient{
		httpClient: httpClient,
		baseURL:    strings.TrimRight(opts.BaseURL, "/"),
		apiKey:     opts.APIKey,
		model:      opts.Model,
	}, nil
}

type rerankRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n,omitempty"`
}

type rerankResponse struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"results"`
}

// Rerank posts (query, docs) to /v1/rerank and returns results sorted by
// score desc. The server already sorts; this implementation re-sorts
// defensively so a future server-side schema drift cannot silently
// invert the ranking signal (the retriever assumes desc order).
//
// Returns:
//   - On success: results in desc-score order, len(results) <= len(docs)
//     (capped by topN when topN > 0).
//   - On transient HTTP failure (timeout via ctx, 429, 5xx, 308): retries
//     once with 100ms backoff then surfaces the error. Caller falls back
//     to the prior stage's top-K.
//   - On permanent failure (400, parse error, empty response): immediate
//     error, no retry.
//   - On len(docs) == 0: returns empty results, nil error.
func (c *modelverseClient) Rerank(ctx context.Context, query string, docs []string, topN int) ([]Result, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(rerankRequest{
		Model:     c.model,
		Query:     query,
		Documents: docs,
		TopN:      topN,
	})
	if err != nil {
		return nil, fmt.Errorf("reranker client: marshal request: %w", err)
	}
	const maxAttempts = 2
	const retryBackoff = 100 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryBackoff):
			}
		}
		results, err := c.doRequest(ctx, body)
		if err == nil {
			return results, nil
		}
		lastErr = err
		var transient *transientError
		if !errors.As(err, &transient) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("reranker client: retry exhausted: %w", lastErr)
}

func (c *modelverseClient) doRequest(ctx context.Context, body []byte) ([]Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("reranker client: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &transientError{err: err}
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &transientError{err: fmt.Errorf("read body: %w", err)}
	}
	if resp.StatusCode == http.StatusOK {
		var parsed rerankResponse
		if err := json.Unmarshal(payload, &parsed); err != nil {
			return nil, fmt.Errorf("reranker client: unmarshal: %w", err)
		}
		if len(parsed.Results) == 0 {
			return nil, errors.New("reranker client: empty results")
		}
		results := make([]Result, 0, len(parsed.Results))
		for _, r := range parsed.Results {
			results = append(results, Result{Index: r.Index, Score: r.RelevanceScore})
		}
		// Server returns desc-sorted; re-sort defensively in case of drift.
		sort.SliceStable(results, func(i, j int) bool {
			return results[i].Score > results[j].Score
		})
		return results, nil
	}
	if isTransientStatus(resp.StatusCode) {
		return nil, &transientError{err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(payload, 200))}
	}
	return nil, fmt.Errorf("reranker client: HTTP %d: %s", resp.StatusCode, truncate(payload, 200))
}

func isTransientStatus(status int) bool {
	switch status {
	case http.StatusPermanentRedirect: // 308
		return true
	case http.StatusTooManyRequests: // 429
		return true
	}
	return status >= 500 && status < 600
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n])
}

type transientError struct{ err error }

func (e *transientError) Error() string { return e.err.Error() }
func (e *transientError) Unwrap() error { return e.err }
