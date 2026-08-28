package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chunkStoreRetriever is a scriptedKnowledgeRetriever that also serves full chunk
// bodies by id, so ReadChunk has something to read. Only the id->chunk map is
// consulted by Chunk; Retrieve behavior is inherited.
type chunkStoreRetriever struct {
	scriptedKnowledgeRetriever
	chunks map[string]knowledge.KBChunk
}

type remoteChunkStoreRetriever struct {
	scriptedKnowledgeRetriever
	chunks map[string]knowledge.KBChunk
	reads  []remoteChunkRead
	err    error
}

type remoteChunkRead struct {
	searchID string
	chunkIDs []string
}

func (r *remoteChunkStoreRetriever) ReadChunks(_ context.Context, searchID string, chunkIDs []string) ([]knowledge.KBChunk, error) {
	r.reads = append(r.reads, remoteChunkRead{searchID: searchID, chunkIDs: append([]string(nil), chunkIDs...)})
	if r.err != nil {
		return nil, r.err
	}
	result := make([]knowledge.KBChunk, 0, len(chunkIDs))
	for _, chunkID := range chunkIDs {
		if chunk, ok := r.chunks[chunkID]; ok {
			result = append(result, chunk)
		}
	}
	return result, nil
}

func (r *chunkStoreRetriever) Chunk(chunkID string) (knowledge.KBChunk, bool) {
	c, ok := r.chunks[strings.TrimSpace(chunkID)]
	return c, ok
}

func readChunkResult(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &out))
	return out
}

func newChunkStoreEngine(t *testing.T, chunks ...knowledge.KBChunk) (*Engine, *chunkStoreRetriever) {
	t.Helper()
	store := map[string]knowledge.KBChunk{}
	for _, c := range chunks {
		store[c.ChunkID] = c
	}
	retriever := &chunkStoreRetriever{chunks: store}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)
	return eng, retriever
}

// The whole point of the tool: SearchKnowledge's snippet stops at 400 runes, so a
// detail past that boundary is invisible until ReadChunk returns the full body.
func TestReadChunk_ReturnsFullBodyBeyondSnippet(t *testing.T) {
	tail := "关键参数：tensor_parallel_size 必须等于 GPU 数量。"
	body := strings.Repeat("前置说明。", 120) + tail // pushes tail past the 400-rune snippet
	require.Greater(t, len([]rune(body)), knowledge.DefaultEvidenceSnippetMaxRunes)
	eng, _ := newChunkStoreEngine(t, knowledge.KBChunk{
		ChunkID: "ext-vllm-tp-001", Title: "vLLM 张量并行", SourceType: "runbook", Content: body,
	})

	out := readChunkResult(t, eng.executeReadChunk(map[string]any{"chunk_ids": []any{"ext-vllm-tp-001"}}, noopStep))
	chunks := out["chunks"].([]any)
	require.Len(t, chunks, 1)
	item := chunks[0].(map[string]any)
	assert.Equal(t, readChunkStatusRead, item["status"])
	assert.Contains(t, item["content"].(string), tail, "the full body carries the detail the snippet cut off")
}

// A second read of the same id in one turn returns already_read, not a duplicate
// body — the dedup that keeps a multi-round loop from re-pasting the same text.
func TestReadChunk_DedupsWithinTurn(t *testing.T) {
	eng, _ := newChunkStoreEngine(t, knowledge.KBChunk{ChunkID: "c1", Content: "完整正文内容。"})

	first := readChunkResult(t, eng.executeReadChunk(map[string]any{"chunk_ids": []any{"c1"}}, noopStep))
	assert.Equal(t, readChunkStatusRead, first["chunks"].([]any)[0].(map[string]any)["status"])

	second := readChunkResult(t, eng.executeReadChunk(map[string]any{"chunk_ids": []any{"c1"}}, noopStep))
	item := second["chunks"].([]any)[0].(map[string]any)
	assert.Equal(t, readChunkStatusAlreadyRead, item["status"])
	assert.Empty(t, item["content"], "an already-read chunk must not re-ship its body")
}

// An unknown id is reported explicitly as not_found rather than silently dropped,
// so the agent learns the id was wrong instead of inferring the chunk was empty.
func TestReadChunk_UnknownIDIsExplicit(t *testing.T) {
	eng, _ := newChunkStoreEngine(t, knowledge.KBChunk{ChunkID: "real", Content: "x"})
	out := readChunkResult(t, eng.executeReadChunk(map[string]any{"chunk_ids": []any{"ghost"}}, noopStep))
	item := out["chunks"].([]any)[0].(map[string]any)
	assert.Equal(t, readChunkStatusNotFound, item["status"])
}

// The per-call id cap truncates the request but records how many were dropped, so
// a partial read never looks like a complete one.
func TestReadChunk_IDCapReportsDropped(t *testing.T) {
	eng, _ := newChunkStoreEngine(t,
		knowledge.KBChunk{ChunkID: "a", Content: "aa"},
		knowledge.KBChunk{ChunkID: "b", Content: "bb"},
		knowledge.KBChunk{ChunkID: "c", Content: "cc"},
		knowledge.KBChunk{ChunkID: "d", Content: "dd"},
	)
	out := readChunkResult(t, eng.executeReadChunk(map[string]any{"chunk_ids": []any{"a", "b", "c", "d"}}, noopStep))
	assert.Len(t, out["chunks"].([]any), maxReadChunkIDsPerCall)
	assert.Equal(t, float64(1), out["dropped_ids"])
}

// The call budget withdraws the tool after maxReadChunkCallsPerTurn; the next call
// returns read_limit_reached instead of another body.
func TestReadChunk_CallBudgetExhausts(t *testing.T) {
	eng, _ := newChunkStoreEngine(t,
		knowledge.KBChunk{ChunkID: "a", Content: "aa"},
		knowledge.KBChunk{ChunkID: "b", Content: "bb"},
		knowledge.KBChunk{ChunkID: "c", Content: "cc"},
	)
	for i := 0; i < maxReadChunkCallsPerTurn; i++ {
		id := string(rune('a' + i))
		_ = eng.executeReadChunk(map[string]any{"chunk_ids": []any{id}}, noopStep)
	}
	out := readChunkResult(t, eng.executeReadChunk(map[string]any{"chunk_ids": []any{"c"}}, noopStep))
	assert.Equal(t, true, out["read_limit_reached"])
}

// A read is recorded as evidence the agent may cite: after ReadChunk, the turn
// ledger carries the chunk with the FULL body as its snippet, not the 400-rune
// search excerpt.
func TestReadChunk_RecordsCitableEvidenceWithFullBody(t *testing.T) {
	tail := "唯一标识串-zzz-END"
	body := strings.Repeat("正文段落。", 120) + tail
	eng, _ := newChunkStoreEngine(t, knowledge.KBChunk{ChunkID: "c1", Title: "T", Content: body})
	// Simulate a prior search that put the chunk in the ledger with a short snippet.
	hit := knowledge.RetrievalHit{Kept: true, Score: 90, Chunk: knowledge.KBChunk{ChunkID: "c1", Title: "T", Content: body}}
	eng.searchKnowledgeLedgerThisTurn = knowledge.BuildSubstantiveEvidenceLedger("q", []knowledge.RetrievalHit{hit}, 3, 0)
	require.NotContains(t, eng.searchKnowledgeLedgerThisTurn.Items[0].Snippet, tail, "precondition: search snippet is truncated before the tail")

	_ = eng.executeReadChunk(map[string]any{"chunk_ids": []any{"c1"}}, noopStep)

	ledger := eng.knowledgeLedgerForVerification("q")
	var snippet string
	for _, item := range ledger.Items {
		if item.ChunkID == "c1" {
			snippet = item.Snippet
		}
	}
	assert.Contains(t, snippet, tail, "the read upgrades the ledger snippet to the full body")
}

// A retriever that cannot serve full bodies (no Chunk method) makes ReadChunk
// report the corpus unavailable rather than panicking.
func TestReadChunk_NoChunkReaderIsUnavailable(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(&scriptedKnowledgeRetriever{})
	out := readChunkResult(t, eng.executeReadChunk(map[string]any{"chunk_ids": []any{"c1"}}, noopStep))
	assert.Contains(t, out["error"].(string), "知识库不可用")
}

func TestReadChunkRemoteUsesOnlyCurrentTurnSearchCapability(t *testing.T) {
	retriever := &remoteChunkStoreRetriever{
		scriptedKnowledgeRetriever: scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
			Enabled:  true,
			SearchID: "current-search-id",
			Hits: []knowledge.KBChunk{{
				ChunkID: "visible", Title: "可见证据", Content: "摘要内容", SourceType: "faq",
			}},
		}}},
		chunks: map[string]knowledge.KBChunk{
			"visible": {ChunkID: "visible", Title: "可见证据", Content: "远程完整正文", ContentTruncated: true, SourceType: "faq"},
			"hidden":  {ChunkID: "hidden", Title: "不可见证据", Content: "不得读取", SourceType: "faq"},
		},
	}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)

	_ = eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "查询可见证据"}, noopStep)
	result := readChunkResult(t, eng.executeReadChunk(map[string]any{"chunk_ids": []any{"visible", "hidden"}}, noopStep))
	items := result["chunks"].([]any)
	require.Len(t, items, 2)
	assert.Equal(t, readChunkStatusRead, items[0].(map[string]any)["status"])
	assert.Equal(t, "远程完整正文", items[0].(map[string]any)["content"])
	assert.Equal(t, true, items[0].(map[string]any)["truncated"], "the model must know a remote full-body response was bounded")
	assert.Equal(t, readChunkStatusSearchNeeded, items[1].(map[string]any)["status"])
	assert.Equal(t, true, result["search_refresh_required"])
	require.Len(t, retriever.reads, 1)
	assert.Equal(t, "current-search-id", retriever.reads[0].searchID)
	assert.Equal(t, []string{"visible"}, retriever.reads[0].chunkIDs)
}

func TestReadChunkRemoteCanReviewBelowFloorCandidateAsLowEvidence(t *testing.T) {
	retriever := &remoteChunkStoreRetriever{
		scriptedKnowledgeRetriever: scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
			Enabled: true, SearchID: "weak-search-id", HybridMode: "qwen3_rrf", RerankerMode: "qwen3-reranker-8b",
			HitItems: []knowledge.RetrievalHit{{
				Kept: true, Score: 0.26,
				Chunk: knowledge.KBChunk{ChunkID: "weak-workbuddy", Title: "WorkBuddy 配置", Content: "搜索节选不应进入证据。"},
			}},
		}}},
		chunks: map[string]knowledge.KBChunk{
			"weak-workbuddy": {ChunkID: "weak-workbuddy", Title: "WorkBuddy 配置", Content: "读取后的完整配置正文。", SourceType: "faq"},
		},
	}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)

	search := readChunkResult(t, eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "WorkBuddy 怎么配置"}, noopStep))
	assert.Empty(t, search["EvidenceLedger"].(map[string]any)["items"])
	require.Len(t, search["below_floor_candidates"].([]any), 1)
	assert.Empty(t, eng.searchKnowledgeLedgerThisTurn.Items)
	assert.Empty(t, eng.searchKnowledgeHitsThisTurn)

	read := readChunkResult(t, eng.executeReadChunk(map[string]any{"chunk_ids": []any{"weak-workbuddy"}}, noopStep))
	item := read["chunks"].([]any)[0].(map[string]any)
	assert.Equal(t, readChunkStatusRead, item["status"])
	assert.Equal(t, "读取后的完整配置正文。", item["content"])
	require.Len(t, retriever.reads, 1)
	assert.Equal(t, "weak-search-id", retriever.reads[0].searchID)

	require.Len(t, eng.searchKnowledgeLedgerThisTurn.Items, 1)
	evidence := eng.searchKnowledgeLedgerThisTurn.Items[0]
	assert.Equal(t, "weak-workbuddy", evidence.ChunkID)
	assert.Equal(t, "low", evidence.ScoreBucket)
	assert.Contains(t, evidence.Snippet, "读取后的完整配置正文")
}

func TestReadChunkRemoteExpiredCapabilityRequiresNewSearch(t *testing.T) {
	retriever := &remoteChunkStoreRetriever{err: fmt.Errorf("%w: test", knowledge.ErrSearchCapabilityInvalid)}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)
	eng.searchKnowledgeCapabilitiesThisTurn = map[string]string{"visible": "expired-search-id"}

	result := readChunkResult(t, eng.executeReadChunk(map[string]any{"chunk_ids": []any{"visible"}}, noopStep))
	item := result["chunks"].([]any)[0].(map[string]any)
	assert.Equal(t, readChunkStatusSearchNeeded, item["status"])
	assert.Equal(t, true, result["search_refresh_required"])
	require.Len(t, retriever.reads, 1, "the reader gets one capability-bound request and never retries arbitrary IDs")
	assert.Equal(t, "expired-search-id", retriever.reads[0].searchID)
}

// ReadChunk is advertised to the agent on the same read-only knowledge lane as
// SearchKnowledge, in both read-only and mutating windows.
func TestReadChunk_AdvertisedOnKnowledgeLane(t *testing.T) {
	// The window also takes the in-instance SSH-ops flag (merged from
	// feat/instance-ops-harness). The knowledge lane must not depend on it, so
	// assert across both values rather than pinning one.
	for _, mutating := range []bool{false, true} {
		for _, instanceOps := range []bool{false, true} {
			names := toolNames(centralAgentToolWindow(mutating, instanceOps))
			assert.Contains(t, names, "ReadChunk")
			assert.Contains(t, names, "SearchKnowledge")
		}
	}
}

func TestReadChunk_EmptyRequestIsRejected(t *testing.T) {
	eng, _ := newChunkStoreEngine(t, knowledge.KBChunk{ChunkID: "c1", Content: "x"})
	out := readChunkResult(t, eng.executeReadChunk(map[string]any{"chunk_ids": []any{}}, noopStep))
	assert.Contains(t, out["error"].(string), "chunk_ids")
}
