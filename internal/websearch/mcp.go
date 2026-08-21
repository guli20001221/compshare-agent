// Package websearch contains the narrow, read-only contract used when the
// curated knowledge base has already been queried and has no usable evidence.
// It intentionally does not fetch result URLs itself: the configured MCP server
// performs the search, and this client returns only bounded, cited candidates to
// the Agent.
package websearch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/compshare-agent/internal/mcpclient"
)

const (
	defaultMaxResults = 5
	maxResults        = 8
	maxQueryRunes     = 512
	maxTitleRunes     = 240
	maxSnippetRunes   = 1200
)

// Result is one externally sourced, linkable observation. URL is required and
// HTTPS-only so a result can be cited to the user without becoming a fetch or
// navigation primitive inside the Agent process.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// Searcher is the engine's narrow dependency. A provider can be changed without
// changing policy: it must expose the MCP search_web result contract below.
type Searcher interface {
	Search(ctx context.Context, query string) ([]Result, error)
}

// MCPOptions configures the external, read-only MCP search adapter. The tool
// contract is deliberately fixed to search_web(query, max_results); the service
// selected by operations must implement that contract rather than leaking a
// provider-specific API into the Agent.
type MCPOptions struct {
	Endpoint    string
	BearerToken string
	Timeout     time.Duration
	HTTPClient  *http.Client
	MaxResults  int
}

// MCPClient is an immutable Searcher safe for process-wide sharing.
type MCPClient struct {
	client     *mcpclient.Client
	maxResults int
}

// NewMCPClient constructs a client but makes no network request. A missing or
// malformed endpoint is rejected at process startup rather than on a user turn.
func NewMCPClient(options MCPOptions) (*MCPClient, error) {
	client, err := mcpclient.New(mcpclient.Options{
		Endpoint:    options.Endpoint,
		BearerToken: options.BearerToken,
		Timeout:     options.Timeout,
		HTTPClient:  options.HTTPClient,
	})
	if err != nil {
		return nil, fmt.Errorf("web search MCP: %w", err)
	}
	limit := options.MaxResults
	if limit == 0 {
		limit = defaultMaxResults
	}
	if limit < 1 || limit > maxResults {
		return nil, fmt.Errorf("web search max_results must be 1..%d, got %d", maxResults, limit)
	}
	return &MCPClient{client: client, maxResults: limit}, nil
}

// Search calls the configured MCP tool and returns only safe, bounded, unique
// HTTPS sources. A malformed item is discarded rather than forwarded as an
// unclickable or private-network link; an empty validated set is a valid search
// result and is not fabricated into an answer.
func (c *MCPClient) Search(ctx context.Context, query string) ([]Result, error) {
	if c == nil || c.client == nil {
		return nil, errors.New("web search MCP client is not configured")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("web search query is required")
	}
	if utf8.RuneCountInString(query) > maxQueryRunes {
		return nil, fmt.Errorf("web search query exceeds %d runes", maxQueryRunes)
	}

	var response struct {
		Results []Result `json:"results"`
	}
	if err := c.client.Call(ctx, "search_web", map[string]any{
		"query":       query,
		"max_results": c.maxResults,
	}, &response); err != nil {
		return nil, err
	}
	return sanitizeResults(response.Results, c.maxResults), nil
}

func sanitizeResults(items []Result, limit int) []Result {
	if limit <= 0 {
		return nil
	}
	result := make([]Result, 0, min(limit, len(items)))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		item.Title = trimRunes(strings.TrimSpace(item.Title), maxTitleRunes)
		item.Snippet = trimRunes(strings.TrimSpace(item.Snippet), maxSnippetRunes)
		address, ok := publicHTTPSURL(item.URL)
		if !ok || item.Title == "" || item.Snippet == "" {
			continue
		}
		if _, exists := seen[address]; exists {
			continue
		}
		seen[address] = struct{}{}
		item.URL = address
		result = append(result, item)
		if len(result) == limit {
			break
		}
	}
	return result
}

func publicHTTPSURL(value string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return "", false
	}
	host := parsed.Hostname()
	lowerHost := strings.ToLower(host)
	if host == "" || strings.EqualFold(host, "localhost") || strings.TrimSuffix(lowerHost, ".local") != lowerHost {
		return "", false
	}
	if ip := net.ParseIP(host); ip != nil {
		address, ok := netip.AddrFromSlice(ip)
		if !ok || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() {
			return "", false
		}
	}
	parsed.Fragment = ""
	return parsed.String(), true
}

func trimRunes(value string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(value) <= limit {
		return value
	}
	return string([]rune(value)[:limit])
}
