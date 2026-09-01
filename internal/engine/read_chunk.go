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

// ReadChunk is the explicit second half of agentic retrieval: SearchKnowledge
// shows a bounded snippet for ordinary/weak entries and may enrich the strongest
// accepted entries; ReadChunk returns any remaining chunk's FULL body by id.
// Without it the agent may have only an excerpt that routinely stops
// before the parameters, thresholds or step list the question was actually about,
// and a truncated excerpt is indistinguishable from a corpus that never covered
// the detail — the agent then denies or guesses instead of reading on.
//
// The snippet default deliberately stays at 400: full text is pay-per-use, not a
// global raise, because every retrieved item is re-fed through the ReAct loop
// while a read is one chunk the agent asked for.
const (
	// maxReadChunkCallsPerTurn bounds how many times the agent may read this turn.
	// It is separate from the SearchKnowledge call budget because a body fetch has
	// its own context cost and must not withdraw search from the tool window.
	maxReadChunkCallsPerTurn = 2
	maxReadChunkIDsPerCall   = 3
	// maxReadChunkRunesPerCall bounds the total body text one call returns. The
	// mean chunk is 1802 runes and the longest in the corpus is 3962, so three
	// typical chunks (~5.4k) fit whole and the cap only bites on unusually long
	// ones — it is a backstop against context blow-up, not a routine truncator.
	// A-RAG's read_chunk has no cap at all and leans on a global token budget we
	// do not have.
	maxReadChunkRunesPerCall = 6000

	// A SearchKnowledge call may enrich only caller-approved strong hits. The
	// budget is owned by the whole turn, not one search call, so repeated
	// searches cannot keep adding full bodies to the model context.
	maxAutoReadChunkIDsPerTurn   = 2
	maxAutoReadChunkRunesPerTurn = 8000
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

type autoReadChunkResult struct {
	ReadIDs      []string
	TruncatedIDs []string
	Unavailable  bool
}

// autoMaterializeKnowledgeChunks enriches a ledger after the central Agent has
// already chosen SearchKnowledge. eligibleIDs is deliberately supplied by the
// caller: score calibration and ranking stay in the search implementation,
// while this helper only enforces the body-read capability and context budget.
//
// Remote reads remain bound to the search_id recorded for each model-visible
// chunk. Local tests/offline evaluation use the in-process Chunk capability.
// Any failed or missing body leaves the original ledger snippet untouched.
func (e *Engine) autoMaterializeKnowledgeChunks(
	ctx context.Context,
	ledger *knowledge.EvidenceLedger,
	eligibleIDs []string,
) autoReadChunkResult {
	result := autoReadChunkResult{ReadIDs: []string{}}
	if e == nil || ledger == nil || len(eligibleIDs) == 0 {
		return result
	}

	if e.automaticKnowledgeBodyIDsThisTurn == nil {
		e.automaticKnowledgeBodyIDsThisTurn = map[string]struct{}{}
	}
	remainingIDs := maxAutoReadChunkIDsPerTurn - len(e.automaticKnowledgeBodyIDsThisTurn)
	runesLeft := maxAutoReadChunkRunesPerTurn - e.automaticKnowledgeBodyRunesThisTurn
	if remainingIDs <= 0 || runesLeft <= 0 {
		return result
	}

	ledgerIndexes := make(map[string]int, len(ledger.Items))
	for i, item := range ledger.Items {
		if id := strings.TrimSpace(item.ChunkID); id != "" {
			ledgerIndexes[id] = i
		}
	}

	ids := make([]string, 0, remainingIDs)
	seen := make(map[string]struct{}, len(eligibleIDs))
	for _, rawID := range eligibleIDs {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		if _, ok := ledgerIndexes[id]; !ok {
			// Local readers do not have a search_id capability, so ledger
			// membership is their equivalent authorization boundary.
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		if _, alreadyRead := e.readChunkIDsThisTurn[id]; alreadyRead {
			continue
		}
		if _, attempted := e.automaticKnowledgeBodyIDsThisTurn[id]; attempted {
			continue
		}
		ids = append(ids, id)
		// Count attempts, not only successful bodies. Repeated searches must not
		// turn a degraded reader into an unbounded remote retry loop.
		e.automaticKnowledgeBodyIDsThisTurn[id] = struct{}{}
		if len(ids) == remainingIDs {
			break
		}
	}
	if len(ids) == 0 {
		return result
	}

	remoteReader, remote := e.knowledgeRetriever.(searchBoundChunkReader)
	localReader, local := e.knowledgeRetriever.(chunkReader)
	if !remote && !local {
		result.Unavailable = true
		return result
	}

	chunksByID := make(map[string]knowledge.KBChunk, len(ids))
	if remote {
		groups := make([]remoteReadGroup, 0, len(ids))
		groupIndex := map[string]int{}
		for _, id := range ids {
			searchID := strings.TrimSpace(e.searchKnowledgeCapabilitiesThisTurn[id])
			if searchID == "" {
				result.Unavailable = true
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
		if ctx == nil {
			ctx = e.currentCtx
			if ctx == nil {
				ctx = context.Background()
			}
		}
		for _, group := range groups {
			chunks, err := remoteReader.ReadChunks(ctx, group.searchID, group.ids)
			if err != nil {
				result.Unavailable = true
				continue
			}
			requested := make(map[string]struct{}, len(group.ids))
			for _, id := range group.ids {
				requested[id] = struct{}{}
			}
			for _, chunk := range chunks {
				id := strings.TrimSpace(chunk.ChunkID)
				if _, ok := requested[id]; ok {
					chunksByID[id] = chunk
				}
			}
		}
	} else {
		for _, id := range ids {
			if chunk, ok := localReader.Chunk(id); ok {
				chunksByID[id] = chunk
			}
		}
	}

	for _, id := range ids {
		chunk, ok := chunksByID[id]
		if !ok {
			result.Unavailable = true
			continue
		}
		content := strings.TrimSpace(chunk.Content)
		if content == "" {
			result.Unavailable = true
			continue
		}
		contentRunes := utf8.RuneCountInString(content)
		truncated := chunk.ContentTruncated || contentRunes > runesLeft
		if contentRunes > runesLeft {
			// truncateRunes appends an ellipsis and is therefore n+1 runes. This
			// boundary is a hard aggregate budget; Truncated already carries the
			// disclosure, so retain exactly runesLeft runes here.
			content = string([]rune(content)[:runesLeft])
			contentRunes = utf8.RuneCountInString(content)
		}
		if contentRunes == 0 {
			break
		}

		ledger.Items[ledgerIndexes[id]].Snippet = content
		e.markChunkRead(id)
		e.automaticKnowledgeBodyRunesThisTurn += contentRunes
		runesLeft -= contentRunes
		result.ReadIDs = append(result.ReadIDs, id)
		if truncated {
			result.TruncatedIDs = append(result.TruncatedIDs, id)
		}
		if runesLeft <= 0 {
			break
		}
	}
	return result
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
	remoteReader, remote := e.knowledgeRetriever.(searchBoundChunkReader)
	localReader, local := e.knowledgeRetriever.(chunkReader)
	if !remote && !local {
		onStep(StepEvent{Type: StepToolResult, Action: "ReadChunk", Source: e.knowledgeToolSource(), Message: "知识库不可用"})
		return readChunkResultJSON(nil, map[string]any{"error": "知识库不可用。"})
	}
	if e.readChunkCallsThisTurn >= maxReadChunkCallsPerTurn {
		onStep(StepEvent{Type: StepToolResult, Action: "ReadChunk", Source: e.knowledgeToolSource(), Message: "本轮读取次数已达上限"})
		return readChunkResultJSON(nil, map[string]any{"read_limit_reached": true})
	}
	e.readChunkCallsThisTurn++
	if remote {
		return e.executeRemoteReadChunk(remoteReader, ids, droppedIDs, onStep)
	}
	items, read := e.materializeReadChunks(ids, nil, localReader.Chunk)
	return e.finishReadChunk(items, read, droppedIDs, nil, "读取完成", onStep)
}

func (e *Engine) executeRemoteReadChunk(reader searchBoundChunkReader, ids []string, droppedIDs int, onStep func(StepEvent)) string {
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

	items, read := e.materializeReadChunks(ids, itemsByID, func(id string) (knowledge.KBChunk, bool) {
		chunk, found := chunksByID[id]
		return chunk, found
	})
	meta := map[string]any{}
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
	return e.finishReadChunk(items, read, droppedIDs, meta, message, onStep)
}

func (e *Engine) materializeReadChunks(
	ids []string,
	itemsByID map[string]readChunkItem,
	lookup func(string) (knowledge.KBChunk, bool),
) ([]readChunkItem, []knowledge.KBChunk) {
	items := make([]readChunkItem, 0, len(ids))
	read := make([]knowledge.KBChunk, 0, len(ids))
	runesLeft := maxReadChunkRunesPerCall
	for _, id := range ids {
		if item, done := itemsByID[id]; done {
			items = append(items, item)
			continue
		}
		if _, seen := e.readChunkIDsThisTurn[id]; seen {
			items = append(items, readChunkItem{ChunkID: id, Status: readChunkStatusAlreadyRead})
			continue
		}
		chunk, found := lookup(id)
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
	return items, read
}

func (e *Engine) finishReadChunk(
	items []readChunkItem,
	read []knowledge.KBChunk,
	droppedIDs int,
	meta map[string]any,
	message string,
	onStep func(StepEvent),
) string {
	e.recordReadChunksAsEvidence(read)
	if meta == nil {
		meta = map[string]any{}
	}
	if droppedIDs > 0 {
		meta["dropped_ids"] = droppedIDs
	}
	traceResult := map[string]any{
		"requested":   len(items) + droppedIDs,
		"read":        len(read),
		"dropped_ids": droppedIDs,
	}
	for key, value := range meta {
		traceResult[key] = value
	}
	onStep(StepEvent{
		Type:        StepToolResult,
		Action:      "ReadChunk",
		Source:      e.knowledgeToolSource(),
		Message:     message,
		TraceResult: traceResult,
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
		score := float64(0)
		if _, belowFloor := e.belowFloorKnowledgeIDsThisTurn[strings.TrimSpace(chunk.ChunkID)]; belowFloor {
			score = 0.01
		}
		hits = append(hits, knowledge.RetrievalHit{Chunk: chunk, Kept: true, Score: score})
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
