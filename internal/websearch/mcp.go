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
	maxExaTextBytes   = 96 << 10

	defaultExaMCPEndpoint = "https://mcp.exa.ai/mcp"
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
	Headers     http.Header
	Timeout     time.Duration
	HTTPClient  *http.Client
	MaxResults  int
}

// MCPClient is an immutable Searcher safe for process-wide sharing.
type MCPClient struct {
	client     *mcpclient.Client
	maxResults int
}

// ExaMCPOptions configures the official Exa-hosted MCP endpoint. Endpoint is
// optional only so the public canonical endpoint can be the secure default;
// any override must still name that exact HTTPS endpoint.
type ExaMCPOptions struct {
	Endpoint   string
	APIKey     string
	Timeout    time.Duration
	HTTPClient *http.Client
	MaxResults int
}

// ExaMCPClient adapts Exa's provider-specific web_search_exa text result to
// the narrow Result contract consumed by the Agent. The rest of the product
// never sees Exa's tool name or response format.
type ExaMCPClient struct {
	client     *mcpclient.Client
	maxResults int
}

// NewMCPClient constructs a client but makes no network request. A missing or
// malformed endpoint is rejected at process startup rather than on a user turn.
func NewMCPClient(options MCPOptions) (*MCPClient, error) {
	client, err := mcpclient.New(mcpclient.Options{
		Endpoint:    options.Endpoint,
		BearerToken: options.BearerToken,
		Headers:     options.Headers,
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

// NewExaMCPClient constructs the provider-specific Exa adapter. Permitting an
// arbitrary endpoint here would let deployment configuration silently turn the
// supposedly Exa-reviewed integration into a different search provider.
func NewExaMCPClient(options ExaMCPOptions) (*ExaMCPClient, error) {
	endpoint := strings.TrimSpace(options.Endpoint)
	if endpoint == "" {
		endpoint = defaultExaMCPEndpoint
	}
	normalized, err := mcpclient.NormalizeEndpoint(endpoint)
	if err != nil {
		return nil, fmt.Errorf("Exa MCP endpoint: %w", err)
	}
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "mcp.exa.ai") || parsed.Path != "/mcp" {
		return nil, fmt.Errorf("Exa MCP endpoint must be %s", defaultExaMCPEndpoint)
	}
	client, err := mcpclient.New(mcpclient.Options{
		Endpoint: normalized, Headers: exaAPIKeyHeader(options.APIKey), Timeout: options.Timeout, HTTPClient: options.HTTPClient,
	})
	if err != nil {
		return nil, fmt.Errorf("Exa MCP: %w", err)
	}
	limit := options.MaxResults
	if limit == 0 {
		limit = defaultMaxResults
	}
	if limit < 1 || limit > maxResults {
		return nil, fmt.Errorf("Exa web-search numResults must be 1..%d, got %d", maxResults, limit)
	}
	return &ExaMCPClient{client: client, maxResults: limit}, nil
}

func exaAPIKeyHeader(apiKey string) http.Header {
	if apiKey = strings.TrimSpace(apiKey); apiKey != "" {
		header := make(http.Header)
		header.Set("x-api-key", apiKey)
		return header
	}
	return nil
}

// Search calls the configured MCP tool and returns only safe, bounded, unique
// HTTPS sources. A malformed item is discarded rather than forwarded as an
// unclickable or private-network link; an empty validated set is a valid search
// result and is not fabricated into an answer.
func (c *MCPClient) Search(ctx context.Context, query string) ([]Result, error) {
	if c == nil || c.client == nil {
		return nil, errors.New("web search MCP client is not configured")
	}
	query, err := validateQuery(query)
	if err != nil {
		return nil, err
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

// Search calls Exa's official web_search_exa tool. Exa returns human-readable
// Title/URL/Highlights blocks rather than a JSON result object, so only
// complete blocks are accepted and every candidate still crosses the same URL
// and size policy as a generic provider result.
func (c *ExaMCPClient) Search(ctx context.Context, query string) ([]Result, error) {
	if c == nil || c.client == nil {
		return nil, errors.New("Exa MCP client is not configured")
	}
	query, err := validateQuery(query)
	if err != nil {
		return nil, err
	}
	text, err := c.client.CallText(ctx, "web_search_exa", map[string]any{
		"query": query, "numResults": c.maxResults,
	})
	if err != nil {
		return nil, err
	}
	if len(text) > maxExaTextBytes {
		return nil, fmt.Errorf("Exa web-search response exceeds %d bytes", maxExaTextBytes)
	}
	return sanitizeResults(parseExaResults(text), c.maxResults), nil
}

func validateQuery(query string) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return "", errors.New("web search query is required")
	}
	if utf8.RuneCountInString(query) > maxQueryRunes {
		return "", fmt.Errorf("web search query exceeds %d runes", maxQueryRunes)
	}
	return query, nil
}

func parseExaResults(text string) []Result {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	blocks := strings.Split(text, "\n---\n")
	results := make([]Result, 0, len(blocks))
	for _, block := range blocks {
		result, ok := parseExaBlock(block)
		if ok {
			results = append(results, result)
		}
	}
	return results
}

func parseExaBlock(block string) (Result, bool) {
	var result Result
	var highlights []string
	inHighlights := false
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if !inHighlights {
			key, value, hasField := strings.Cut(trimmed, ":")
			switch {
			case hasField && key == "Title" && result.Title == "":
				result.Title = strings.TrimSpace(value)
			case hasField && key == "URL" && result.URL == "":
				result.URL = strings.TrimSpace(value)
			case hasField && key == "Highlights" && strings.TrimSpace(value) == "":
				inHighlights = true
			}
			continue
		}
		if trimmed != "" && trimmed != "..." {
			highlights = append(highlights, trimmed)
		}
	}
	result.Snippet = strings.Join(highlights, "\n")
	return result, result.Title != "" && result.URL != "" && result.Snippet != ""
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
