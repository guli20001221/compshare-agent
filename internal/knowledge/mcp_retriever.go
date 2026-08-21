package knowledge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/compshare-agent/internal/mcpclient"
)

// ErrSearchCapabilityInvalid means the short-lived search_id presented to the
// remote knowledge service can no longer authorize a read. Callers must issue a
// new search; they must not retry a read against an arbitrary chunk ID.
var ErrSearchCapabilityInvalid = errors.New("knowledge: search capability is invalid or expired")

// MCPRetrieverOptions configures the read-only client for compshare-kb's
// streamable HTTP MCP endpoint. BearerToken is optional because deployments may
// protect an in-cluster endpoint with network policy instead; it is never used
// for the management /admin/mcp endpoint.
type MCPRetrieverOptions struct {
	Endpoint    string
	BearerToken string
	Timeout     time.Duration
	HTTPClient  *http.Client
}

// MCPRetriever is the remote adapter behind the existing retrieval seam. It
// contains only immutable connection configuration, so one instance is safe to
// share through engine.SharedDeps. Per-search capabilities deliberately remain
// in the calling Engine's current-turn state.
type MCPRetriever struct {
	client *mcpclient.Client
}

// NewMCPRetriever validates and normalizes an endpoint. A bare in-cluster DNS
// name is accepted for deployment ergonomics and becomes an http URL; an empty
// path becomes /mcp.
func NewMCPRetriever(options MCPRetrieverOptions) (*MCPRetriever, error) {
	client, err := mcpclient.New(mcpclient.Options{
		Endpoint:    options.Endpoint,
		BearerToken: options.BearerToken,
		Timeout:     options.Timeout,
		HTTPClient:  options.HTTPClient,
	})
	if err != nil {
		return nil, err
	}
	return &MCPRetriever{client: client}, nil
}

// RetrieveContext searches the active remote knowledge release and projects its
// bounded snippets onto the agent's existing RetrievalResult shape.
func (r *MCPRetriever) RetrieveContext(ctx context.Context, question, contextHint string) RetrievalResult {
	question = strings.TrimSpace(question)
	if r == nil || question == "" {
		return RetrievalResult{Enabled: true, Empty: true, Unavailable: true, FailureReason: "mcp_not_configured"}
	}

	var response mcpSearchResponse
	if err := r.callTool(ctx, "search_knowledge", map[string]any{
		"query":        question,
		"context_hint": strings.TrimSpace(contextHint),
	}, &response); err != nil {
		return unavailableMCPRetrieval(question, err)
	}
	if strings.TrimSpace(response.Release.Version) == "" {
		return unavailableMCPRetrieval(question, errors.New("search response omitted kb_version"))
	}

	hits := make([]KBChunk, 0, len(response.Hits))
	hitItems := make([]RetrievalHit, 0, len(response.Hits))
	for _, hit := range response.Hits {
		chunkID := strings.TrimSpace(hit.ChunkID)
		if chunkID == "" {
			return unavailableMCPRetrieval(question, errors.New("search response contained an empty chunk_id"))
		}
		chunk := KBChunk{
			ChunkID:      chunkID,
			KBVersion:    strings.TrimSpace(response.Release.Version),
			SourceType:   strings.TrimSpace(hit.SourceType),
			SourceOrigin: strings.TrimSpace(hit.SourceOrigin),
			ProductArea:  strings.TrimSpace(hit.ProductArea),
			Title:        strings.TrimSpace(hit.Title),
			Content:      strings.TrimSpace(hit.Snippet),
			SurfaceURL:   cloneStringPtr(hit.SurfaceURL),
		}
		hits = append(hits, chunk)
		hitItems = append(hitItems, RetrievalHit{Chunk: chunk, Score: hit.Score, Kept: true})
	}

	result := RetrievalResult{
		Enabled:                true,
		KBVersion:              strings.TrimSpace(response.Release.Version),
		QueryNormalized:        NormalizeQuery(question),
		Hits:                   hits,
		HitItems:               hitItems,
		Empty:                  response.Empty || len(hits) == 0,
		SearchID:               strings.TrimSpace(response.SearchID),
		HybridMode:             strings.TrimSpace(response.Retrieval.Mode),
		HybridFallbackReason:   strings.TrimSpace(response.Retrieval.FallbackReason),
		EmbeddingLatencyMS:     response.Retrieval.EmbeddingLatencyMS,
		EmbeddingModel:         strings.TrimSpace(response.Retrieval.EmbeddingModel),
		RerankerMode:           strings.TrimSpace(response.Retrieval.RerankerModel),
		RerankerLatencyMS:      response.Retrieval.RerankerLatencyMS,
		RerankerFallbackReason: strings.TrimSpace(response.Retrieval.RerankerFallbackReason),
	}
	result.HybridMode, result.HybridFallbackReason = normalizeRemoteScoreScale(result.HybridMode, result.HybridFallbackReason)
	if result.Empty {
		result.SearchID = ""
	}
	return result
}

// ReadChunks reads full evidence only through the search capability returned by
// the immediately preceding search. It deliberately accepts a searchID rather
// than retaining one on MCPRetriever, preventing cross-turn or cross-session
// capability reuse.
func (r *MCPRetriever) ReadChunks(ctx context.Context, searchID string, chunkIDs []string) ([]KBChunk, error) {
	if r == nil {
		return nil, errors.New("knowledge MCP retriever is not configured")
	}
	searchID = strings.TrimSpace(searchID)
	chunkIDs = uniqueChunkIDs(chunkIDs)
	if searchID == "" || len(chunkIDs) == 0 {
		return nil, fmt.Errorf("%w: search_id and chunk_ids are required", ErrSearchCapabilityInvalid)
	}

	var response mcpReadResponse
	if err := r.callTool(ctx, "read_knowledge_chunk", map[string]any{
		"search_id": searchID,
		"chunk_ids": chunkIDs,
	}, &response); err != nil {
		return nil, classifyMCPReadError(err)
	}
	if strings.TrimSpace(response.Release.Version) == "" {
		return nil, errors.New("knowledge MCP read response omitted kb_version")
	}

	items := make([]KBChunk, 0, len(response.Items))
	for _, item := range response.Items {
		chunkID := strings.TrimSpace(item.ChunkID)
		if chunkID == "" {
			return nil, errors.New("knowledge MCP read response contained an empty chunk_id")
		}
		items = append(items, KBChunk{
			ChunkID:          chunkID,
			KBVersion:        strings.TrimSpace(response.Release.Version),
			SourceType:       strings.TrimSpace(item.SourceType),
			SourceOrigin:     strings.TrimSpace(item.SourceOrigin),
			ProductArea:      strings.TrimSpace(item.ProductArea),
			Title:            strings.TrimSpace(item.Title),
			Content:          strings.TrimSpace(item.Content),
			ContentTruncated: item.Truncated,
			SurfaceURL:       cloneStringPtr(item.SurfaceURL),
		})
	}
	return items, nil
}

// normalizeRemoteScoreScale decides whether this process may judge the remote's
// scores against a relevance floor, and says so in the mode itself.
//
// It exists because the previous behavior — defaulting an absent mode to
// bm25_only and calling that "BM25-safe" — is only safe when the scores really
// are BM25 scores. The BM25 floor is 55.0 and a reranker/cosine scale tops out
// at 1.0, so guessing wrong in that direction rejects EVERY hit, empties the
// ledger, and reads downstream as "the corpus has nothing" rather than as a
// missing metadata field. That is the same failure the qwen3_rrf reranker
// fallback already caused once (see isWeakEvidence in internal/engine), and it
// gets the same answer: when the scale is not identifiable, decline to judge
// rather than judge on a guess.
//
// An UNRECOGNIZED mode is treated exactly like an absent one, and deliberately
// so: compshare-kb owns its own retrieval pipeline and can rename or add a mode
// without this repo shipping, which makes "a mode we have never calibrated"
// more likely over time than "no mode at all".
//
// The raw value is never dropped — it is preserved in the fallback reason so an
// operator can see which unknown mode disabled the floor.
func normalizeRemoteScoreScale(mode, fallbackReason string) (string, string) {
	mode = strings.TrimSpace(mode)
	if KnownRetrievalMode(mode) {
		return mode, fallbackReason
	}
	reason := "remote_mode_unrecognized:" + mode
	if mode == "" {
		reason = "remote_mode_missing"
	}
	if trimmed := strings.TrimSpace(fallbackReason); trimmed != "" {
		// Keep whatever the remote said about its own degradation; this marker is
		// additional information, not a replacement for it.
		reason = trimmed + "; " + reason
	}
	return RetrievalModeUnknownRemote, reason
}

func unavailableMCPRetrieval(question string, err error) RetrievalResult {
	return RetrievalResult{
		Enabled:         true,
		QueryNormalized: NormalizeQuery(question),
		Empty:           true,
		Unavailable:     true,
		FailureReason:   mcpFailureReason(err),
	}
}

func (r *MCPRetriever) callTool(ctx context.Context, name string, arguments map[string]any, output any) error {
	if r == nil || r.client == nil {
		return errors.New("knowledge MCP retriever is not configured")
	}
	return r.client.Call(ctx, name, arguments, output)
}

func classifyMCPReadError(err error) error {
	if err == nil {
		return nil
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "search capability") ||
		strings.Contains(message, "search_id") ||
		strings.Contains(message, "chunk was not returned") ||
		strings.Contains(message, "not returned by the referenced search") ||
		strings.Contains(message, "knowledge evidence token") ||
		strings.Contains(message, "token has expired") ||
		strings.Contains(message, "token is invalid") {
		return fmt.Errorf("%w: %v", ErrSearchCapabilityInvalid, err)
	}
	return err
}

func mcpFailureReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "mcp_timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "mcp_canceled"
	}
	return "mcp_unavailable"
}

func normalizeMCPEndpoint(value string) (string, error) {
	return mcpclient.NormalizeEndpoint(value)
}

func uniqueChunkIDs(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func cloneStringPtr(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

type mcpRelease struct {
	ID      string `json:"release_id"`
	Version string `json:"kb_version"`
}

type mcpRetrievalMeta struct {
	Mode                   string `json:"mode"`
	EmbeddingModel         string `json:"embedding_model"`
	RerankerModel          string `json:"reranker_model"`
	FallbackReason         string `json:"fallback_reason"`
	RerankerFallbackReason string `json:"reranker_fallback_reason"`
	EmbeddingLatencyMS     *int64 `json:"embedding_latency_ms"`
	RerankerLatencyMS      *int64 `json:"reranker_latency_ms"`
}

type mcpEvidenceHit struct {
	ChunkID      string  `json:"chunk_id"`
	Title        string  `json:"title"`
	Snippet      string  `json:"snippet"`
	Score        float64 `json:"score"`
	SourceType   string  `json:"source_type"`
	SourceOrigin string  `json:"source_origin"`
	ProductArea  string  `json:"product_area"`
	SurfaceURL   *string `json:"surface_url"`
}

type mcpSearchResponse struct {
	SearchID  string           `json:"search_id"`
	Release   mcpRelease       `json:"release"`
	Hits      []mcpEvidenceHit `json:"hits"`
	Empty     bool             `json:"empty"`
	Retrieval mcpRetrievalMeta `json:"retrieval"`
}

type mcpReadChunk struct {
	ChunkID      string  `json:"chunk_id"`
	Title        string  `json:"title"`
	Content      string  `json:"content"`
	Truncated    bool    `json:"truncated,omitempty"`
	SourceType   string  `json:"source_type"`
	SourceOrigin string  `json:"source_origin"`
	ProductArea  string  `json:"product_area"`
	SurfaceURL   *string `json:"surface_url"`
}

type mcpReadResponse struct {
	Release mcpRelease     `json:"release"`
	Items   []mcpReadChunk `json:"items"`
}
