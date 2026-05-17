package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Embedder is implemented by anything that returns a 3072-dim float32 vector
// for a single text input. The hybrid retriever depends only on this interface
// so unit tests can inject deterministic vectors without an HTTP server.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// ClientOptions wires a ModelVerse-style /v1/embeddings client. Timeout is
// per-call (request + read); set 0 to inherit the http.Client default.
type ClientOptions struct {
	BaseURL    string
	APIKey     string
	Model      string
	HTTPClient *http.Client
	Timeout    time.Duration
}

// Client is an OpenAI-compatible embeddings client. The hybrid retriever uses
// it for query embeddings only — corpus embeddings are pre-built offline by
// scripts/rag_w0/build_corpus_embeddings.py and pinned via
// knowledge.EmbeddingDigestExpected.
type Client struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
	model      string
}

// NewClient validates configuration and returns a ready-to-use Client.
func NewClient(opts ClientOptions) (*Client, error) {
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, errors.New("embedding client: APIKey is required")
	}
	if strings.TrimSpace(opts.BaseURL) == "" {
		return nil, errors.New("embedding client: BaseURL is required")
	}
	if strings.TrimSpace(opts.Model) == "" {
		return nil, errors.New("embedding client: Model is required")
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		timeout := opts.Timeout
		if timeout <= 0 {
			// CLI smoke against production ModelVerse (2026-05-17) showed
			// embedding p99 well above 1s; settled on 5s as a default that
			// keeps fallback rare in steady state but still bounds tail
			// latency. Override via ClientOptions.Timeout when calling.
			timeout = 5 * time.Second
		}
		httpClient = &http.Client{Timeout: timeout}
	}
	return &Client{
		httpClient: httpClient,
		baseURL:    strings.TrimRight(opts.BaseURL, "/"),
		apiKey:     opts.APIKey,
		model:      opts.Model,
	}, nil
}

// Embed returns the 3072-dim vector for text. On transient failures (timeout,
// 429, 5xx, 308) it retries once with a fixed 100ms backoff then surfaces the
// error so the caller (retriever hybrid branch) can fall back to BM25 top-3.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	const maxAttempts = 2
	const retryBackoff = 100 * time.Millisecond

	body, err := json.Marshal(map[string]any{
		"model": c.model,
		"input": []string{text},
	})
	if err != nil {
		return nil, fmt.Errorf("embedding client: marshal request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(retryBackoff):
			}
		}
		vec, err := c.doRequest(ctx, body)
		if err == nil {
			return vec, nil
		}
		lastErr = err
		var transient *transientError
		if !errors.As(err, &transient) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("embedding client: retry exhausted: %w", lastErr)
}

func (c *Client) doRequest(ctx context.Context, body []byte) ([]float32, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embedding client: new request: %w", err)
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
		var parsed embedResponse
		if err := json.Unmarshal(payload, &parsed); err != nil {
			return nil, fmt.Errorf("embedding client: unmarshal: %w", err)
		}
		if len(parsed.Data) == 0 || len(parsed.Data[0].Embedding) == 0 {
			return nil, errors.New("embedding client: empty embedding")
		}
		return parsed.Data[0].Embedding, nil
	}
	if isTransientStatus(resp.StatusCode) {
		return nil, &transientError{err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(payload, 200))}
	}
	return nil, fmt.Errorf("embedding client: HTTP %d: %s", resp.StatusCode, truncate(payload, 200))
}

func isTransientStatus(status int) bool {
	switch status {
	case http.StatusPermanentRedirect: // 308 — ModelVerse occasionally returns this
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

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
}

type transientError struct{ err error }

func (e *transientError) Error() string { return e.err.Error() }
func (e *transientError) Unwrap() error { return e.err }
