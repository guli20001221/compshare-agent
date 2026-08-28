package sshops

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/security"
)

const (
	maxKnowledgeSearchCallsPerRun = 4
	maxKnowledgeReadCallsPerRun   = 2
	maxKnowledgeSearchHits        = 3
	// Match the outer ReadChunk contract: each of the at-most-two read calls
	// may request three chunks and returns at most 6000 runes in total.
	maxKnowledgeReadChunks       = 3
	maxKnowledgeReadRunes        = 6000
	maxKnowledgeQueryRunes       = 1024
	maxKnowledgeContextHintRunes = 200
	maxKnowledgeRequestIDRunes   = 128
	maxKnowledgeChunkIDRunes     = 256
	maxKnowledgeProductAreaRunes = 80
)

// KnowledgeRetriever is the read-only retrieval seam shared with the outer
// Agent. Keeping the interface here avoids coupling the SSH transport package
// to engine while allowing the same production MCPRetriever instance to serve
// both lanes. Connection configuration and search capabilities never cross the
// harness process boundary.
type KnowledgeRetriever interface {
	RetrieveContext(ctx context.Context, question, productArea string) knowledge.RetrievalResult
}

type knowledgeRemoteChunkReader interface {
	ReadChunks(ctx context.Context, searchID string, chunkIDs []string) ([]knowledge.KBChunk, error)
}

type knowledgeLocalChunkReader interface {
	Chunk(chunkID string) (knowledge.KBChunk, bool)
}

// KnowledgeRequest is emitted by the harness as an @@KNOWLEDGE side-band line.
// It deliberately has no search_id field: the Go broker owns that short-lived
// capability and will read only chunk ids returned by a search in this run.
type KnowledgeRequest struct {
	ID          string   `json:"id"`
	Operation   string   `json:"operation"`
	Query       string   `json:"query,omitempty"`
	ContextHint string   `json:"context_hint,omitempty"`
	ChunkIDs    []string `json:"chunk_ids,omitempty"`
}

type knowledgeReply struct {
	ID         string `json:"id"`
	OK         bool   `json:"ok"`
	Result     any    `json:"result,omitempty"`
	ErrorClass string `json:"error_class,omitempty"`
}

type knowledgeSearchHit struct {
	ChunkID     string `json:"chunk_id"`
	Title       string `json:"title,omitempty"`
	SourceType  string `json:"source_type,omitempty"`
	ProductArea string `json:"product_area,omitempty"`
	Snippet     string `json:"snippet,omitempty"`
}

type knowledgeSearchResult struct {
	KBVersion string               `json:"kb_version,omitempty"`
	Hits      []knowledgeSearchHit `json:"hits"`
	Empty     bool                 `json:"empty"`
}

type knowledgeReadItem struct {
	ChunkID     string `json:"chunk_id"`
	Title       string `json:"title,omitempty"`
	SourceType  string `json:"source_type,omitempty"`
	ProductArea string `json:"product_area,omitempty"`
	Content     string `json:"content,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
}

type knowledgeReadResult struct {
	Chunks          []knowledgeReadItem `json:"chunks"`
	MissingChunkIDs []string            `json:"missing_chunk_ids,omitempty"`
}

type knowledgeReadCapability struct {
	searchID string
}

// knowledgeBridge is created once per Supervisor.RunWithContext. Its counters
// and chunk capabilities therefore cannot cross turns, sessions or tenants,
// even though the underlying retriever is an immutable process-wide client.
type knowledgeBridge struct {
	ctx          context.Context
	retriever    KnowledgeRetriever
	searchCalls  int
	readCalls    int
	capabilities map[string]knowledgeReadCapability
}

func newKnowledgeBridge(ctx context.Context, retriever KnowledgeRetriever) *knowledgeBridge {
	if ctx == nil {
		ctx = context.Background()
	}
	return &knowledgeBridge{
		ctx:          ctx,
		retriever:    retriever,
		capabilities: make(map[string]knowledgeReadCapability),
	}
}

func (b *knowledgeBridge) handle(req KnowledgeRequest) knowledgeReply {
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" || utf8.RuneCountInString(req.ID) > maxKnowledgeRequestIDRunes {
		return knowledgeFailure(req.ID, "invalid_request")
	}
	switch strings.TrimSpace(req.Operation) {
	case "search":
		return b.search(req)
	case "read":
		return b.read(req)
	default:
		return knowledgeFailure(req.ID, "invalid_request")
	}
}

func (b *knowledgeBridge) search(req KnowledgeRequest) knowledgeReply {
	rawQuery := strings.TrimSpace(req.Query)
	rawHint := strings.TrimSpace(req.ContextHint)
	if rawQuery == "" || utf8.RuneCountInString(rawQuery) > maxKnowledgeQueryRunes ||
		utf8.RuneCountInString(rawHint) > maxKnowledgeContextHintRunes {
		return knowledgeFailure(req.ID, "invalid_request")
	}
	query := strings.TrimSpace(security.RedactUserConversationText(rawQuery))
	hint := strings.TrimSpace(security.RedactUserConversationText(rawHint))
	if b == nil || b.retriever == nil {
		return knowledgeFailure(req.ID, "unavailable")
	}
	if b.searchCalls >= maxKnowledgeSearchCallsPerRun {
		return knowledgeFailure(req.ID, "limit_exceeded")
	}
	b.searchCalls++

	retrieved := b.retriever.RetrieveContext(b.ctx, query, hint)
	if retrieved.Unavailable {
		return knowledgeFailure(req.ID, "unavailable")
	}
	hits := retrieved.HitItems
	if len(hits) == 0 && len(retrieved.Hits) > 0 {
		hits = make([]knowledge.RetrievalHit, 0, len(retrieved.Hits))
		for _, chunk := range retrieved.Hits {
			hits = append(hits, knowledge.RetrievalHit{Chunk: chunk, Kept: true})
		}
	}
	ledger := knowledge.BuildSubstantiveEvidenceLedger(query, hits,
		maxKnowledgeSearchHits, knowledge.DefaultEvidenceSnippetMaxRunes)
	resultHits := make([]knowledgeSearchHit, 0, len(ledger.Items))
	for _, item := range ledger.Items {
		chunkID := strings.TrimSpace(item.ChunkID)
		if chunkID == "" || utf8.RuneCountInString(chunkID) > maxKnowledgeChunkIDRunes {
			continue
		}
		b.capabilities[chunkID] = knowledgeReadCapability{searchID: strings.TrimSpace(retrieved.SearchID)}
		resultHits = append(resultHits, knowledgeSearchHit{
			ChunkID:     chunkID,
			Title:       item.Title,
			SourceType:  item.SourceType,
			ProductArea: truncateRunes(strings.TrimSpace(item.ProductArea), maxKnowledgeProductAreaRunes),
			Snippet:     item.Snippet,
		})
	}
	return knowledgeReply{
		ID: req.ID,
		OK: true,
		Result: knowledgeSearchResult{
			KBVersion: strings.TrimSpace(retrieved.KBVersion),
			Hits:      resultHits,
			Empty:     retrieved.Empty || len(resultHits) == 0,
		},
	}
}

func (b *knowledgeBridge) read(req KnowledgeRequest) knowledgeReply {
	ids := uniqueKnowledgeChunkIDs(req.ChunkIDs)
	if len(ids) == 0 {
		return knowledgeFailure(req.ID, "invalid_request")
	}
	if len(ids) > maxKnowledgeReadChunks {
		return knowledgeFailure(req.ID, "limit_exceeded")
	}
	if b == nil || b.retriever == nil {
		return knowledgeFailure(req.ID, "unavailable")
	}
	if b.readCalls >= maxKnowledgeReadCallsPerRun {
		return knowledgeFailure(req.ID, "limit_exceeded")
	}
	for _, id := range ids {
		if _, ok := b.capabilities[id]; !ok {
			return knowledgeFailure(req.ID, "not_authorized")
		}
	}
	b.readCalls++

	chunks, err := b.readAuthorized(ids)
	if err != nil {
		if errors.Is(err, knowledge.ErrSearchCapabilityInvalid) {
			return knowledgeFailure(req.ID, "not_authorized")
		}
		return knowledgeFailure(req.ID, "unavailable")
	}
	byID := make(map[string]knowledge.KBChunk, len(chunks))
	for _, chunk := range chunks {
		if id := strings.TrimSpace(chunk.ChunkID); id != "" {
			byID[id] = chunk
		}
	}

	runesLeft := maxKnowledgeReadRunes
	items := make([]knowledgeReadItem, 0, len(ids))
	missing := make([]string, 0)
	for _, id := range ids {
		chunk, ok := byID[id]
		if !ok {
			missing = append(missing, id)
			continue
		}
		content := strings.TrimSpace(chunk.Content)
		truncated := chunk.ContentTruncated || utf8.RuneCountInString(content) > runesLeft
		if runesLeft <= 0 {
			content = ""
		} else if truncated {
			content = truncateRunes(content, runesLeft)
		}
		runesLeft -= utf8.RuneCountInString(content)
		items = append(items, knowledgeReadItem{
			ChunkID:     id,
			Title:       truncateRunes(strings.TrimSpace(chunk.Title), 80),
			SourceType:  truncateRunes(strings.TrimSpace(chunk.SourceType), 40),
			ProductArea: truncateRunes(strings.TrimSpace(chunk.ProductArea), maxKnowledgeProductAreaRunes),
			Content:     content,
			Truncated:   truncated,
		})
	}
	return knowledgeReply{ID: req.ID, OK: true, Result: knowledgeReadResult{
		Chunks: items, MissingChunkIDs: missing,
	}}
}

func (b *knowledgeBridge) readAuthorized(ids []string) ([]knowledge.KBChunk, error) {
	if remote, ok := b.retriever.(knowledgeRemoteChunkReader); ok {
		groups := make(map[string][]string)
		order := make([]string, 0)
		for _, id := range ids {
			searchID := b.capabilities[id].searchID
			if searchID == "" {
				return nil, knowledge.ErrSearchCapabilityInvalid
			}
			if _, seen := groups[searchID]; !seen {
				order = append(order, searchID)
			}
			groups[searchID] = append(groups[searchID], id)
		}
		var chunks []knowledge.KBChunk
		for _, searchID := range order {
			group, err := remote.ReadChunks(b.ctx, searchID, groups[searchID])
			if err != nil {
				return nil, err
			}
			chunks = append(chunks, group...)
		}
		return chunks, nil
	}
	if local, ok := b.retriever.(knowledgeLocalChunkReader); ok {
		chunks := make([]knowledge.KBChunk, 0, len(ids))
		for _, id := range ids {
			if chunk, found := local.Chunk(id); found {
				chunks = append(chunks, chunk)
			}
		}
		return chunks, nil
	}
	return nil, errors.New("knowledge retriever does not support chunk reads")
}

func uniqueKnowledgeChunkIDs(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || utf8.RuneCountInString(value) > maxKnowledgeChunkIDRunes {
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

func knowledgeFailure(id, class string) knowledgeReply {
	return knowledgeReply{ID: strings.TrimSpace(id), OK: false, ErrorClass: class}
}
