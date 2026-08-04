package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/observability"
)

// ReadChunk is the second half of agentic retrieval: SearchKnowledge FINDS chunks
// and shows a bounded snippet of each (DefaultEvidenceSnippetMaxRunes = 400 runes,
// about 22% of the mean 1802-rune chunk), ReadChunk returns one chunk's FULL body
// by id. Without it the agent must answer from an excerpt that routinely stops
// before the parameters, thresholds or step list the question was actually about,
// and a truncated excerpt is indistinguishable from a corpus that never covered
// the detail — the agent then denies or guesses instead of reading on.
//
// The snippet default deliberately stays at 400: full text is pay-per-use, not a
// global raise, because every retrieved item is re-fed through the ReAct loop
// while a read is one chunk the agent asked for.
const (
	// maxReadChunkCallsPerTurn bounds how many times the agent may read this turn.
	// A read is not a retrieval, so it is NOT charged to maxRetrievalQueriesPerTurn:
	// that budget prices search/rerank round-trips, while a read is a separately
	// bounded evidence-body fetch (remote MCP in production) with its own context
	// cap. Mixing them would let reading starve the multi-hop search it serves.
	maxReadChunkCallsPerTurn = 2
	maxReadChunkIDsPerCall   = 3
	// maxReadChunkRunesPerCall bounds the total body text one call returns. The
	// mean chunk is 1802 runes and the longest in the corpus is 3962, so three
	// typical chunks (~5.4k) fit whole and the cap only bites on unusually long
	// ones — it is a backstop against context blow-up, not a routine truncator.
	// A-RAG's read_chunk has no cap at all and leans on a global token budget we
	// do not have.
	maxReadChunkRunesPerCall = 6000
)

const (
	readChunkStatusRead         = "read"
	readChunkStatusAlreadyRead  = "already_read"
	readChunkStatusNotFound     = "not_found"
	readChunkStatusSizeLimit    = "size_limit_reached"
	readChunkStatusSearchNeeded = "search_required"
	readChunkStatusUnavailable  = "unavailable"
)

// chunkReader is the optional capability a KnowledgeRetriever may implement to
// serve a full chunk body by id. It is kept OFF the KnowledgeRetriever interface
// so every existing implementation and test double still satisfies it; a
// retriever without the method simply makes ReadChunk report the corpus
// unavailable rather than failing to compile.
type chunkReader interface {
	Chunk(chunkID string) (knowledge.KBChunk, bool)
}

// searchBoundChunkReader is the remote half of knowledge retrieval. The Engine
// supplies the search_id it recorded for this turn; a reader never retains it,
// which prevents a capability from leaking between sessions or turns.
type searchBoundChunkReader interface {
	ReadChunks(ctx context.Context, searchID string, chunkIDs []string) ([]knowledge.KBChunk, error)
}

type readChunkItem struct {
	ChunkID    string `json:"chunk_id"`
	Status     string `json:"status"`
	Title      string `json:"title,omitempty"`
	SourceType string `json:"source_type,omitempty"`
	Content    string `json:"content,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
}

// executeReadChunk runs the ReadChunk tool. It is read-only by construction:
// local development reads the in-process corpus, while production reads only a
// capability-authorized body from MCP. It never touches SafeToolExecutor or a
// mutating endpoint.
func (e *Engine) executeReadChunk(args map[string]any, onStep func(StepEvent)) string {
	ids := readChunkIDArgs(args)
	onStep(StepEvent{
		Type:   StepToolCall,
		Action: "ReadChunk",
		Source: e.knowledgeToolSource(),
		Args:   map[string]any{"chunk_ids": append([]string(nil), ids...)},
	})
	if len(ids) == 0 {
		onStep(StepEvent{Type: StepToolResult, Action: "ReadChunk", Source: e.knowledgeToolSource(), Message: "缺少 chunk_id"})
		return readChunkResultJSON(nil, map[string]any{"error": "chunk_ids 不能为空，请填入 SearchKnowledge 返回过的 chunk_id。"})
	}
	// Never drop a requested id silently: a truncated read must not look like a
	// complete one.
	droppedIDs := 0
	if len(ids) > maxReadChunkIDsPerCall {
		droppedIDs = len(ids) - maxReadChunkIDsPerCall
		ids = ids[:maxReadChunkIDsPerCall]
	}
	if reader, ok := e.knowledgeRetriever.(searchBoundChunkReader); ok {
		return e.executeRemoteReadChunk(reader, ids, droppedIDs, onStep)
	}
	reader, ok := e.knowledgeRetriever.(chunkReader)
	if !ok {
		onStep(StepEvent{Type: StepToolResult, Action: "ReadChunk", Source: e.knowledgeToolSource(), Message: "知识库不可用"})
		return readChunkResultJSON(nil, map[string]any{"error": "知识库不可用。"})
	}
	if e.readChunkCallsThisTurn >= maxReadChunkCallsPerTurn {
		onStep(StepEvent{Type: StepToolResult, Action: "ReadChunk", Source: e.knowledgeToolSource(), Message: "本轮读取次数已达上限"})
		return readChunkResultJSON(nil, map[string]any{"read_limit_reached": true})
	}
	e.readChunkCallsThisTurn++

	items := make([]readChunkItem, 0, len(ids))
	read := make([]knowledge.KBChunk, 0, len(ids))
	runesLeft := maxReadChunkRunesPerCall
	for _, id := range ids {
		if _, seen := e.readChunkIDsThisTurn[id]; seen {
			// Already in the conversation from an earlier read this turn. Re-sending
			// the body would duplicate it in context for no new information.
			items = append(items, readChunkItem{ChunkID: id, Status: readChunkStatusAlreadyRead})
			continue
		}
		chunk, found := reader.Chunk(id)
		if !found {
			items = append(items, readChunkItem{ChunkID: id, Status: readChunkStatusNotFound})
			continue
		}
		if runesLeft <= 0 {
			items = append(items, readChunkItem{ChunkID: id, Status: readChunkStatusSizeLimit})
			continue
		}
		content := strings.TrimSpace(chunk.Content)
		truncated := chunk.ContentTruncated || utf8.RuneCountInString(content) > runesLeft
		if truncated {
			content = truncateRunes(content, runesLeft)
		}
		runesLeft -= utf8.RuneCountInString(content)
		e.markChunkRead(id)
		read = append(read, chunk)
		items = append(items, readChunkItem{
			ChunkID:    id,
			Status:     readChunkStatusRead,
			Title:      strings.TrimSpace(chunk.Title),
			SourceType: strings.TrimSpace(chunk.SourceType),
			Content:    content,
			Truncated:  truncated,
		})
	}
	e.recordReadChunksAsEvidence(read)

	meta := map[string]any{}
	if droppedIDs > 0 {
		meta["dropped_ids"] = droppedIDs
	}
	onStep(StepEvent{
		Type:        StepToolResult,
		Action:      "ReadChunk",
		Source:      e.knowledgeToolSource(),
		Message:     "读取完成",
		TraceResult: map[string]any{"requested": len(items) + droppedIDs, "read": len(read), "dropped_ids": droppedIDs},
	})
	return readChunkResultJSON(items, meta)
}

func (e *Engine) executeRemoteReadChunk(reader searchBoundChunkReader, ids []string, droppedIDs int, onStep func(StepEvent)) string {
	if e.readChunkCallsThisTurn >= maxReadChunkCallsPerTurn {
		onStep(StepEvent{Type: StepToolResult, Action: "ReadChunk", Source: e.knowledgeToolSource(), Message: "本轮读取次数已达上限"})
		return readChunkResultJSON(nil, map[string]any{"read_limit_reached": true})
	}
	e.readChunkCallsThisTurn++

	itemsByID := make(map[string]readChunkItem, len(ids))
	groups := make([]remoteReadGroup, 0, len(ids))
	groupIndex := map[string]int{}
	searchRefreshNeeded := false
	for _, id := range ids {
		if _, seen := e.readChunkIDsThisTurn[id]; seen {
			itemsByID[id] = readChunkItem{ChunkID: id, Status: readChunkStatusAlreadyRead}
			continue
		}
		searchID := ""
		if e.searchKnowledgeCapabilitiesThisTurn != nil {
			searchID = strings.TrimSpace(e.searchKnowledgeCapabilitiesThisTurn[id])
		}
		if searchID == "" {
			// Do not send a guessed chunk_id to the remote service. A model may
			// only read an ID that appeared in its own SearchKnowledge result.
			itemsByID[id] = readChunkItem{ChunkID: id, Status: readChunkStatusSearchNeeded}
			searchRefreshNeeded = true
			continue
		}
		index, ok := groupIndex[searchID]
		if !ok {
			index = len(groups)
			groupIndex[searchID] = index
			groups = append(groups, remoteReadGroup{searchID: searchID})
		}
		groups[index].ids = append(groups[index].ids, id)
	}

	ctx := e.currentCtx
	if ctx == nil {
		ctx = context.Background()
	}
	chunksByID := make(map[string]knowledge.KBChunk, len(ids))
	remoteUnavailable := false
	for _, group := range groups {
		chunks, err := reader.ReadChunks(ctx, group.searchID, group.ids)
		if err != nil {
			if errors.Is(err, knowledge.ErrSearchCapabilityInvalid) {
				searchRefreshNeeded = true
				for _, id := range group.ids {
					itemsByID[id] = readChunkItem{ChunkID: id, Status: readChunkStatusSearchNeeded}
				}
				continue
			}
			remoteUnavailable = true
			for _, id := range group.ids {
				itemsByID[id] = readChunkItem{ChunkID: id, Status: readChunkStatusUnavailable}
			}
			continue
		}
		for _, chunk := range chunks {
			chunkID := strings.TrimSpace(chunk.ChunkID)
			if chunkID != "" {
				chunksByID[chunkID] = chunk
			}
		}
	}

	items := make([]readChunkItem, 0, len(ids))
	read := make([]knowledge.KBChunk, 0, len(ids))
	runesLeft := maxReadChunkRunesPerCall
	for _, id := range ids {
		if item, done := itemsByID[id]; done {
			items = append(items, item)
			continue
		}
		chunk, found := chunksByID[id]
		if !found {
			items = append(items, readChunkItem{ChunkID: id, Status: readChunkStatusNotFound})
			continue
		}
		if runesLeft <= 0 {
			items = append(items, readChunkItem{ChunkID: id, Status: readChunkStatusSizeLimit})
			continue
		}
		content := strings.TrimSpace(chunk.Content)
		truncated := chunk.ContentTruncated || utf8.RuneCountInString(content) > runesLeft
		if truncated {
			content = truncateRunes(content, runesLeft)
		}
		runesLeft -= utf8.RuneCountInString(content)
		e.markChunkRead(id)
		read = append(read, chunk)
		items = append(items, readChunkItem{
			ChunkID:    id,
			Status:     readChunkStatusRead,
			Title:      strings.TrimSpace(chunk.Title),
			SourceType: strings.TrimSpace(chunk.SourceType),
			Content:    content,
			Truncated:  truncated,
		})
	}
	e.recordReadChunksAsEvidence(read)

	meta := map[string]any{}
	if droppedIDs > 0 {
		meta["dropped_ids"] = droppedIDs
	}
	if searchRefreshNeeded {
		meta["search_refresh_required"] = true
	}
	if remoteUnavailable {
		meta["knowledge_unavailable"] = true
	}
	message := "读取完成"
	if remoteUnavailable {
		message = "知识库服务暂时不可用"
	} else if searchRefreshNeeded {
		message = "检索凭证已失效，请重新搜索"
	}
	onStep(StepEvent{
		Type:        StepToolResult,
		Action:      "ReadChunk",
		Source:      e.knowledgeToolSource(),
		Message:     message,
		TraceResult: map[string]any{"requested": len(items) + droppedIDs, "read": len(read), "dropped_ids": droppedIDs, "search_refresh_required": searchRefreshNeeded, "knowledge_unavailable": remoteUnavailable},
	})
	return readChunkResultJSON(items, meta)
}

type remoteReadGroup struct {
	searchID string
	ids      []string
}

func (e *Engine) knowledgeToolSource() string {
	if _, ok := e.knowledgeRetriever.(searchBoundChunkReader); ok {
		return observability.ToolSourceKnowledgeMCP
	}
	return observability.ToolSourceKnowledgeLocal
}

func (e *Engine) markChunkRead(chunkID string) {
	if e.readChunkIDsThisTurn == nil {
		e.readChunkIDsThisTurn = map[string]struct{}{}
	}
	e.readChunkIDsThisTurn[chunkID] = struct{}{}
}

// recordReadChunksAsEvidence folds full-body reads into the same per-turn evidence
// the citation check runs against, so an answer may cite a chunk it READ as well
// as one it searched. The ledger snippet is upgraded to the read text: the ledger
// must mirror what the agent actually saw, or the recovery synthesis would
// re-summarize a 400-rune excerpt of a body the agent read in full.
func (e *Engine) recordReadChunksAsEvidence(chunks []knowledge.KBChunk) {
	if len(chunks) == 0 {
		return
	}
	hits := make([]knowledge.RetrievalHit, 0, len(chunks))
	for _, chunk := range chunks {
		hits = append(hits, knowledge.RetrievalHit{Chunk: chunk, Kept: true})
	}
	e.searchKnowledgeHitsThisTurn = append(e.searchKnowledgeHitsThisTurn, hits...)
	question := e.searchKnowledgeLedgerThisTurn.Query
	readLedger := knowledge.BuildSubstantiveEvidenceLedger(question, hits, len(hits), maxReadChunkRunesPerCall)
	e.searchKnowledgeLedgerThisTurn = knowledge.MergeEvidenceLedgers(
		e.searchKnowledgeLedgerThisTurn, readLedger, searchKnowledgeLedgerTurnMaxItems)
	// MergeEvidenceLedgers keeps the first item per chunk_id, so a chunk already
	// present from its search keeps the 400-rune snippet. Overwrite it.
	snippets := map[string]string{}
	for _, item := range readLedger.Items {
		snippets[item.ChunkID] = item.Snippet
	}
	for i, item := range e.searchKnowledgeLedgerThisTurn.Items {
		if snippet, ok := snippets[item.ChunkID]; ok && snippet != "" {
			e.searchKnowledgeLedgerThisTurn.Items[i].Snippet = snippet
		}
	}
}

// readChunkIDArgs extracts the requested ids. It accepts the declared array form
// and a comma/whitespace-separated string, because that is a deterministic
// normalization of the same value — not a guess about intent.
func readChunkIDArgs(args map[string]any) []string {
	var raw []string
	switch v := args["chunk_ids"].(type) {
	case []any:
		for _, entry := range v {
			if s, ok := entry.(string); ok {
				raw = append(raw, s)
			}
		}
	case []string:
		raw = append(raw, v...)
	case string:
		raw = strings.FieldsFunc(v, func(r rune) bool {
			return r == ',' || r == '，' || r == ' ' || r == '\n' || r == '\t'
		})
	}
	out := make([]string, 0, len(raw))
	seen := map[string]struct{}{}
	for _, id := range raw {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func readChunkResultJSON(items []readChunkItem, meta map[string]any) string {
	if items == nil {
		items = []readChunkItem{}
	}
	result := map[string]any{"chunks": items}
	for k, v := range meta {
		result[k] = v
	}
	b, err := json.Marshal(result)
	if err != nil {
		return `{"chunks":[],"error":"读取结果序列化失败。"}`
	}
	return string(b)
}
