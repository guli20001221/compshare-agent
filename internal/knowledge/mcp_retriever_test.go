package knowledge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPRetrieverSearchAndRead(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test-kb", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "search_knowledge"}, func(_ context.Context, _ *mcp.CallToolRequest, input mcpSearchInput) (*mcp.CallToolResult, mcpSearchResponse, error) {
		if input.Query != "GPU 内存不够怎么办" {
			return nil, mcpSearchResponse{}, errors.New("unexpected query")
		}
		if input.ContextHint != "gpu" {
			return nil, mcpSearchResponse{}, errors.New("unexpected context hint")
		}
		return nil, mcpSearchResponse{
			SearchID: "search-capability",
			Release:  mcpRelease{ID: "release-42", Version: "kb.compshare.2026-08-03"},
			Retrieval: mcpRetrievalMeta{
				Mode:                   RetrievalModeQwen3RRF,
				EmbeddingModel:         "qwen3-embedding-8b",
				RerankerModel:          "qwen3-reranker-8b",
				EmbeddingLatencyMS:     int64Ptr(12),
				RerankerLatencyMS:      int64Ptr(34),
				RerankerFallbackReason: "",
			},
			Hits: []mcpEvidenceHit{{
				ChunkID:      "gpu-001",
				Title:        "GPU 显存排查",
				Snippet:      "先检查显存占用。",
				Score:        0.91,
				SourceType:   "runbook",
				SourceOrigin: "official",
				ProductArea:  "gpu",
			}},
		}, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "read_knowledge_chunk"}, func(_ context.Context, _ *mcp.CallToolRequest, input mcpReadInput) (*mcp.CallToolResult, mcpReadResponse, error) {
		if input.SearchID == "expired" {
			return nil, mcpReadResponse{}, errors.New("knowledge evidence token has expired")
		}
		if input.SearchID != "search-capability" || len(input.ChunkIDs) != 1 || input.ChunkIDs[0] != "gpu-001" {
			return nil, mcpReadResponse{}, errors.New("knowledge MCP: chunk was not returned by the referenced search")
		}
		return nil, mcpReadResponse{
			Release: mcpRelease{ID: "release-42", Version: "kb.compshare.2026-08-03"},
			Items: []mcpReadChunk{{
				ChunkID:      "gpu-001",
				Title:        "GPU 显存排查",
				Content:      "先检查显存占用，再减少 batch size。",
				Truncated:    true,
				SourceType:   "runbook",
				SourceOrigin: "official",
				ProductArea:  "gpu",
			}},
		}, nil
	})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer read-token" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(writer, request)
	}))
	defer httpServer.Close()

	retriever, err := NewMCPRetriever(MCPRetrieverOptions{
		Endpoint:    httpServer.URL,
		BearerToken: "read-token",
		Timeout:     time.Second,
	})
	require.NoError(t, err)

	result := retriever.RetrieveContext(context.Background(), "GPU 内存不够怎么办", "gpu")
	require.False(t, result.Unavailable)
	require.False(t, result.Empty)
	assert.Equal(t, "search-capability", result.SearchID)
	assert.Equal(t, "kb.compshare.2026-08-03", result.KBVersion)
	assert.Equal(t, RetrievalModeQwen3RRF, result.HybridMode)
	require.Len(t, result.HitItems, 1)
	assert.Equal(t, "先检查显存占用。", result.HitItems[0].Chunk.Content)
	assert.Equal(t, "official", result.HitItems[0].Chunk.SourceOrigin)

	items, err := retriever.ReadChunks(context.Background(), result.SearchID, []string{"gpu-001"})
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "先检查显存占用，再减少 batch size。", items[0].Content)
	assert.True(t, items[0].ContentTruncated, "the MCP read-response truncation flag must reach the agent")
	assert.Equal(t, "kb.compshare.2026-08-03", items[0].KBVersion)

	_, err = retriever.ReadChunks(context.Background(), "expired", []string{"gpu-001"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSearchCapabilityInvalid)
}

func TestMCPRetrieverUnavailableIsNotAnEmptySearch(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "not ready", http.StatusServiceUnavailable)
	}))
	defer httpServer.Close()

	retriever, err := NewMCPRetriever(MCPRetrieverOptions{Endpoint: httpServer.URL, Timeout: time.Second})
	require.NoError(t, err)

	result := retriever.RetrieveContext(context.Background(), "查询知识", "")
	assert.True(t, result.Unavailable)
	assert.Equal(t, "mcp_unavailable", result.FailureReason)
	assert.Empty(t, result.Hits)
}

func TestNormalizeMCPEndpoint(t *testing.T) {
	endpoint, err := normalizeMCPEndpoint(" compshare-kb.prj-ucompshare-prod.svc.c5.u4 ")
	require.NoError(t, err)
	assert.Equal(t, "http://compshare-kb.prj-ucompshare-prod.svc.c5.u4/mcp", endpoint)

	endpoint, err = normalizeMCPEndpoint("https://kb.example/mcp/")
	require.NoError(t, err)
	assert.Equal(t, "https://kb.example/mcp", endpoint)

	for _, invalid := range []string{"", "ftp://kb.example/mcp", "http://kb.example/mcp?debug=1", "http:///mcp"} {
		_, err := normalizeMCPEndpoint(invalid)
		assert.Error(t, err, invalid)
	}
}

type mcpSearchInput struct {
	Query       string `json:"query"`
	ContextHint string `json:"context_hint"`
}

type mcpReadInput struct {
	SearchID string   `json:"search_id"`
	ChunkIDs []string `json:"chunk_ids"`
}

func int64Ptr(value int64) *int64 { return &value }

func TestClassifyMCPReadError(t *testing.T) {
	for _, source := range []string{
		"knowledge MCP: chunk was not returned by the referenced search",
		"knowledge evidence token has expired",
		"knowledge evidence token is invalid",
	} {
		err := classifyMCPReadError(errors.New(source))
		assert.ErrorIs(t, err, ErrSearchCapabilityInvalid)
		assert.True(t, strings.Contains(err.Error(), source))
	}
}
