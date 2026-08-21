package websearch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPClientSearchUsesFixedToolContractAndFiltersUnsafeSources(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test-search", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "search_web"}, func(_ context.Context, _ *mcp.CallToolRequest, input searchInput) (*mcp.CallToolResult, searchOutput, error) {
		if input.Query != "PyTorch CUDA 版本兼容" {
			return nil, searchOutput{}, errors.New("unexpected query")
		}
		if input.MaxResults != 3 {
			return nil, searchOutput{}, errors.New("unexpected max_results")
		}
		return nil, searchOutput{Results: []Result{
			{Title: "PyTorch previous versions", URL: "https://pytorch.org/get-started/previous-versions/#cuda", Snippet: "官方 CUDA 兼容矩阵。"},
			{Title: "duplicate", URL: "https://pytorch.org/get-started/previous-versions/#other", Snippet: "同一页不能重复。"},
			{Title: "insecure", URL: "http://example.com", Snippet: "不能交给用户。"},
			{Title: "loopback", URL: "https://127.0.0.1/internal", Snippet: "不能泄露私网链接。"},
		}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true,
	})
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer search-token" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		if request.Header.Get("X-Provider-Key") != "provider-test-key" {
			http.Error(writer, "missing provider header", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(writer, request)
	}))
	defer httpServer.Close()

	client, err := NewMCPClient(MCPOptions{
		Endpoint: httpServer.URL, BearerToken: "search-token", Timeout: time.Second, MaxResults: 3,
		// This verifies the shared MCP transport can safely carry a
		// provider-specific header without changing its bearer behavior.
		// Exa maps its API key to x-api-key through the same path.
		Headers: http.Header{"X-Provider-Key": []string{"provider-test-key"}},
	})
	require.NoError(t, err)

	results, err := client.Search(context.Background(), "PyTorch CUDA 版本兼容")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "PyTorch previous versions", results[0].Title)
	assert.Equal(t, "https://pytorch.org/get-started/previous-versions/", results[0].URL,
		"fragments must not turn one source into multiple citations")
}

func TestExaMCPClientUsesProviderToolAndFiltersFormattedText(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test-exa", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "web_search_exa"}, func(_ context.Context, _ *mcp.CallToolRequest, input exaSearchInput) (*mcp.CallToolResult, struct{}, error) {
		if input.Query != "优云智算 文档" {
			return nil, struct{}{}, errors.New("unexpected query")
		}
		if input.NumResults != 3 {
			return nil, struct{}{}, errors.New("unexpected numResults")
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `Title: 优云智算文档
URL: https://www.compshare.cn/docs/guide#one
Published: N/A
Highlights:
首条可引用资料。
...

---

Title: 重复页面
URL: https://www.compshare.cn/docs/guide#two
Highlights:
同一地址不能重复。

---

Title: 不安全地址
URL: http://example.com/not-for-users
Highlights:
应被丢弃。

---

Title: 没有摘要
URL: https://example.com/empty
Published: N/A`}}}, struct{}{}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true,
	})
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	transport, err := mcpclient.New(mcpclient.Options{Endpoint: httpServer.URL, Timeout: time.Second})
	require.NoError(t, err)
	client := &ExaMCPClient{client: transport, maxResults: 3}

	results, err := client.Search(context.Background(), "优云智算 文档")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "优云智算文档", results[0].Title)
	assert.Equal(t, "https://www.compshare.cn/docs/guide", results[0].URL)
	assert.Equal(t, "首条可引用资料。", results[0].Snippet)
}

func TestExaAPIKeyHeaderUsesProviderRequiredName(t *testing.T) {
	header := exaAPIKeyHeader(" exa-test-key ")
	assert.Equal(t, "exa-test-key", header.Get("x-api-key"))
	assert.Empty(t, header.Get("Authorization"))
}

func TestNewExaMCPClientPinsTheOfficialEndpoint(t *testing.T) {
	client, err := NewExaMCPClient(ExaMCPOptions{})
	require.NoError(t, err)
	require.NotNil(t, client)

	_, err = NewExaMCPClient(ExaMCPOptions{Endpoint: "https://search.example/mcp"})
	require.ErrorContains(t, err, "https://mcp.exa.ai/mcp")
}

// TestExaMCPClientLive is intentionally opt-in: it proves the production
// transport against Exa without making normal CI depend on a third-party rate
// limit. Its query is fixed public documentation text, never a user turn.
func TestExaMCPClientLive(t *testing.T) {
	if os.Getenv("COMPSHARE_EXA_LIVE_TEST") != "1" {
		t.Skip("set COMPSHARE_EXA_LIVE_TEST=1 to probe Exa's hosted MCP")
	}
	client, err := NewExaMCPClient(ExaMCPOptions{Timeout: 20 * time.Second, MaxResults: 3})
	require.NoError(t, err)
	results, err := client.Search(context.Background(), "优云智算 官方文档")
	require.NoError(t, err)
	require.NotEmpty(t, results)
	for _, result := range results {
		assert.True(t, strings.HasPrefix(result.URL, "https://"))
		assert.NotEmpty(t, result.Title)
		assert.NotEmpty(t, result.Snippet)
	}
}

func TestMCPClientRejectsInvalidLimitsAndQueries(t *testing.T) {
	_, err := NewMCPClient(MCPOptions{Endpoint: "https://search.example/mcp", MaxResults: maxResults + 1})
	require.Error(t, err)

	client, err := NewMCPClient(MCPOptions{Endpoint: "https://search.example/mcp"})
	require.NoError(t, err)
	_, err = client.Search(context.Background(), "")
	require.Error(t, err)
	_, err = client.Search(context.Background(), string(make([]rune, maxQueryRunes+1)))
	require.Error(t, err)
}

func TestSanitizeResultsLimitsAndRequiresSourceText(t *testing.T) {
	results := sanitizeResults([]Result{
		{Title: "", URL: "https://example.com/a", Snippet: "no title"},
		{Title: "no snippet", URL: "https://example.com/b", Snippet: ""},
		{Title: "one", URL: "https://example.com/one", Snippet: "first"},
		{Title: "two", URL: "https://example.org/two", Snippet: "second"},
	}, 1)
	require.Len(t, results, 1)
	assert.Equal(t, "one", results[0].Title)
}

type searchInput struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

type searchOutput struct {
	Results []Result `json:"results"`
}

type exaSearchInput struct {
	Query      string `json:"query"`
	NumResults int    `json:"numResults"`
}
