package websearch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
		handler.ServeHTTP(writer, request)
	}))
	defer httpServer.Close()

	client, err := NewMCPClient(MCPOptions{
		Endpoint: httpServer.URL, BearerToken: "search-token", Timeout: time.Second, MaxResults: 3,
	})
	require.NoError(t, err)

	results, err := client.Search(context.Background(), "PyTorch CUDA 版本兼容")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "PyTorch previous versions", results[0].Title)
	assert.Equal(t, "https://pytorch.org/get-started/previous-versions/", results[0].URL,
		"fragments must not turn one source into multiple citations")
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
