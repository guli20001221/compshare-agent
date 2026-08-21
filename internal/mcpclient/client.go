// Package mcpclient provides the small read-only Streamable HTTP MCP transport
// shared by integrations that consume an MCP tool.  It deliberately knows
// nothing about a tool's schema or product policy: callers own both.
package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultRequestTimeout bounds one remote MCP invocation unless its consumer
// explicitly chooses a smaller or larger value.
const DefaultRequestTimeout = 12 * time.Second

// Options configures one immutable read-only MCP client. BearerToken is
// optional for trusted in-cluster endpoints. Headers supports a small number of
// provider-required headers (for example Exa's x-api-key); neither is logged by
// this package.
type Options struct {
	Endpoint    string
	BearerToken string
	Headers     http.Header
	Timeout     time.Duration
	HTTPClient  *http.Client
}

// Client contains only immutable connection settings and is safe to share.
type Client struct {
	endpoint   string
	timeout    time.Duration
	httpClient *http.Client
}

// New validates and normalizes an endpoint. A bare host is accepted for
// in-cluster ergonomics and gets http://; an empty path becomes /mcp.
func New(options Options) (*Client, error) {
	endpoint, err := NormalizeEndpoint(options.Endpoint)
	if err != nil {
		return nil, err
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	return &Client{
		endpoint:   endpoint,
		timeout:    timeout,
		httpClient: cloneHTTPClientWithAuth(options.HTTPClient, options.BearerToken, options.Headers),
	}, nil
}

// Call invokes one MCP tool and decodes structured content (or a JSON text
// content fallback) into output. It does not retry: tool semantics own whether
// an invocation is safe to repeat.
func (c *Client) Call(ctx context.Context, name string, arguments map[string]any, output any) error {
	result, err := c.callTool(ctx, name, arguments)
	if err != nil {
		return err
	}
	if err := DecodeToolResult(result, output); err != nil {
		return fmt.Errorf("decode MCP tool %q: %w", strings.TrimSpace(name), err)
	}
	return nil
}

// CallText invokes one MCP tool and returns its non-empty text content. It is
// deliberately separate from Call: callers that consume a provider-owned text
// format must parse it themselves instead of treating arbitrary text as JSON.
func (c *Client) CallText(ctx context.Context, name string, arguments map[string]any) (string, error) {
	result, err := c.callTool(ctx, name, arguments)
	if err != nil {
		return "", err
	}
	text, err := toolResultTextValue(result)
	if err != nil {
		return "", fmt.Errorf("decode MCP tool %q text: %w", strings.TrimSpace(name), err)
	}
	return text, nil
}

func (c *Client) callTool(ctx context.Context, name string, arguments map[string]any) (*mcp.CallToolResult, error) {
	if c == nil {
		return nil, errors.New("MCP client is not configured")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("MCP tool name is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "compshare-agent",
		Version: "1.0.0",
	}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             c.endpoint,
		HTTPClient:           c.httpClient,
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect MCP: %w", err)
	}
	defer func() { _ = session.Close() }()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		return nil, fmt.Errorf("call MCP tool %q: %w", name, err)
	}
	return result, nil
}

// NormalizeEndpoint validates a Streamable HTTP MCP endpoint. It accepts http
// only for trusted in-cluster deployments; callers deciding on public endpoints
// may impose a stricter HTTPS policy separately.
func NormalizeEndpoint(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("MCP endpoint is required")
	}
	if _, _, hasSchemeSeparator := strings.Cut(value, "://"); !hasSchemeSeparator {
		value = "http://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("parse MCP endpoint: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("MCP endpoint scheme %q must be http or https", parsed.Scheme)
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return "", errors.New("MCP endpoint host is required")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("MCP endpoint must not contain user credentials, query, or fragment")
	}
	path := strings.TrimRight(parsed.Path, "/")
	if path == "" {
		parsed.Path = "/mcp"
	} else {
		parsed.Path = path
	}
	parsed.RawPath = ""
	return parsed.String(), nil
}

// DecodeToolResult is exported for consumers that need to verify a result
// shape in isolation. MCP tool errors remain errors rather than a successful
// zero-value decode.
func DecodeToolResult(result *mcp.CallToolResult, output any) error {
	if result == nil {
		return errors.New("empty tool result")
	}
	if result.IsError {
		return &toolError{message: toolResultText(result)}
	}
	if result.StructuredContent != nil {
		encoded, err := json.Marshal(result.StructuredContent)
		if err != nil {
			return fmt.Errorf("marshal structured content: %w", err)
		}
		if err := json.Unmarshal(encoded, output); err != nil {
			return fmt.Errorf("unmarshal structured content: %w", err)
		}
		return nil
	}
	for _, content := range result.Content {
		text, ok := content.(*mcp.TextContent)
		if !ok || strings.TrimSpace(text.Text) == "" {
			continue
		}
		if err := json.Unmarshal([]byte(text.Text), output); err != nil {
			return fmt.Errorf("unmarshal text content: %w", err)
		}
		return nil
	}
	return errors.New("tool result did not include structured or JSON text content")
}

type toolError struct{ message string }

func (e *toolError) Error() string {
	if e == nil || strings.TrimSpace(e.message) == "" {
		return "MCP tool returned an error"
	}
	return strings.TrimSpace(e.message)
}

func toolResultText(result *mcp.CallToolResult) string {
	text, _ := toolResultTextContent(result)
	return text
}

func toolResultTextValue(result *mcp.CallToolResult) (string, error) {
	if result == nil {
		return "", errors.New("empty tool result")
	}
	if result.IsError {
		message := toolResultText(result)
		if message == "" {
			message = "MCP tool returned an error"
		}
		return "", &toolError{message: message}
	}
	return toolResultTextContent(result)
}

func toolResultTextContent(result *mcp.CallToolResult) (string, error) {
	if result == nil {
		return "", errors.New("empty tool result")
	}
	texts := make([]string, 0, len(result.Content))
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok && strings.TrimSpace(text.Text) != "" {
			texts = append(texts, strings.TrimSpace(text.Text))
		}
	}
	if len(texts) == 0 {
		return "", errors.New("tool result did not include text content")
	}
	return strings.Join(texts, "\n"), nil
}

func cloneHTTPClientWithAuth(base *http.Client, bearerToken string, headers http.Header) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	cloned := *base
	if strings.TrimSpace(bearerToken) != "" || len(headers) > 0 {
		transport := cloned.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		cloned.Transport = authRoundTripper{
			next:        transport,
			bearerToken: strings.TrimSpace(bearerToken),
			headers:     headers.Clone(),
		}
	}
	return &cloned
}

type authRoundTripper struct {
	next        http.RoundTripper
	bearerToken string
	headers     http.Header
}

func (t authRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	cloned.Header = request.Header.Clone()
	for name, values := range t.headers {
		cloned.Header.Del(name)
		for _, value := range values {
			cloned.Header.Add(name, value)
		}
	}
	if t.bearerToken != "" {
		cloned.Header.Set("Authorization", "Bearer "+t.bearerToken)
	}
	return t.next.RoundTrip(cloned)
}
