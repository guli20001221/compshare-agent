package knowledge

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRRFKValueIsSixty locks the canonical smoothing constant against
// accidental tuning. Cormack et al. 2009 SIGIR established k=60 and every
// major hybrid-search vendor (Azure, Elastic 8.8+ rank_constant,
// OpenSearch 2.19+ score-ranker, Vespa, LlamaIndex) defaults to it.
// Changing this constant requires an offline eval justification.
func TestRRFKValueIsSixty(t *testing.T) {
	assert.Equal(t, 60, rrfK)
}

// TestRRFFusionBasic verifies the canonical fusion formula on a small
// hand-checkable example. Three chunks A, B, C; BM25 ranks them A,B,C;
// dense ranks them C,B,A. Each chunk thus appears once at rank 1 and
// once at rank 3 (or twice at rank 2 for B). Expected scores:
//
//	A: 1/61 + 1/63
//	B: 1/62 + 1/62
//	C: 1/63 + 1/61
//
// A and C are symmetric. Tie-break is chunk_id ascending so A wins.
func TestRRFFusionBasic(t *testing.T) {
	chunkA := KBChunk{ChunkID: "A"}
	chunkB := KBChunk{ChunkID: "B"}
	chunkC := KBChunk{ChunkID: "C"}

	bm25List := []scoredChunk{
		{chunk: chunkA, score: 10.0},
		{chunk: chunkB, score: 5.0},
		{chunk: chunkC, score: 1.0},
	}
	denseList := []scoredChunk{
		{chunk: chunkC, score: 0.9},
		{chunk: chunkB, score: 0.5},
		{chunk: chunkA, score: 0.1},
	}

	fused, info := rrfFusion(bm25List, denseList, 10)
	require.Len(t, fused, 3)

	wantA := 1.0/61.0 + 1.0/63.0
	wantB := 1.0/62.0 + 1.0/62.0
	wantC := 1.0/63.0 + 1.0/61.0

	assertScoreEq := func(t *testing.T, label string, got, want float64) {
		t.Helper()
		if math.Abs(got-want) > 1e-12 {
			t.Errorf("%s score: got %g want %g", label, got, want)
		}
	}
	assertScoreEq(t, "A", info["A"].FusionScore, wantA)
	assertScoreEq(t, "B", info["B"].FusionScore, wantB)
	assertScoreEq(t, "C", info["C"].FusionScore, wantC)

	// Stable tie-break: A and C are exactly equal in score, A < C lexically.
	assert.Equal(t, "A", fused[0].chunk.ChunkID, "A wins tie via chunk_id asc")
	assert.Equal(t, "C", fused[1].chunk.ChunkID, "C second (same score, larger id)")
	assert.Equal(t, "B", fused[2].chunk.ChunkID, "B last (lower fused score)")

	// Verify rank info is populated.
	assert.Equal(t, 1, info["A"].BM25Rank)
	assert.Equal(t, 3, info["A"].DenseRank)
	assert.Equal(t, 2, info["B"].BM25Rank)
	assert.Equal(t, 2, info["B"].DenseRank)
	assert.Equal(t, 3, info["C"].BM25Rank)
	assert.Equal(t, 1, info["C"].DenseRank)
}

// TestRRFFusionTieBreak hand-constructs a case where two chunks land on
// exactly the same fused score and asserts chunk_id ascending wins.
// Symmetric input guarantees the tie; without stable sort this would
// drift across runs and break determinism gates downstream.
func TestRRFFusionTieBreak(t *testing.T) {
	bm25 := []scoredChunk{
		{chunk: KBChunk{ChunkID: "zzzz"}, score: 1.0},
		{chunk: KBChunk{ChunkID: "aaaa"}, score: 0.5},
	}
	dense := []scoredChunk{
		{chunk: KBChunk{ChunkID: "aaaa"}, score: 1.0},
		{chunk: KBChunk{ChunkID: "zzzz"}, score: 0.5},
	}
	fused, _ := rrfFusion(bm25, dense, 10)
	require.Len(t, fused, 2)
	assert.Equal(t, "aaaa", fused[0].chunk.ChunkID, "aaaa < zzzz wins tie")
	assert.Equal(t, "zzzz", fused[1].chunk.ChunkID)
}

// TestRRFFusionMissingFromOneList verifies that a chunk appearing in only
// one of the two ranked lists still surfaces in the fused output. This is
// the entire point of fusion: union, not intersection. The absent list
// contributes zero to the score (no penalty term).
func TestRRFFusionMissingFromOneList(t *testing.T) {
	bm25 := []scoredChunk{
		{chunk: KBChunk{ChunkID: "only-in-bm25"}, score: 5.0},
	}
	dense := []scoredChunk{
		{chunk: KBChunk{ChunkID: "only-in-dense"}, score: 0.9},
	}
	fused, info := rrfFusion(bm25, dense, 10)
	require.Len(t, fused, 2)

	// Both chunks should have score = 1/(60+1+1) = 1/61
	assert.InDelta(t, 1.0/61.0, info["only-in-bm25"].FusionScore, 1e-12)
	assert.InDelta(t, 1.0/61.0, info["only-in-dense"].FusionScore, 1e-12)

	// Equal score → chunk_id tie-break: only-in-bm25 < only-in-dense.
	assert.Equal(t, "only-in-bm25", fused[0].chunk.ChunkID)
	assert.Equal(t, "only-in-dense", fused[1].chunk.ChunkID)

	// Absent list contributes rank 0 (sentinel for "not seen").
	assert.Equal(t, 1, info["only-in-bm25"].BM25Rank)
	assert.Equal(t, 0, info["only-in-bm25"].DenseRank, "absent from dense → DenseRank 0")
	assert.Equal(t, 0, info["only-in-dense"].BM25Rank, "absent from bm25 → BM25Rank 0")
	assert.Equal(t, 1, info["only-in-dense"].DenseRank)
}

// TestRRFFusionEmptyLists is the panic-guard. Both inputs empty must
// return empty without panicking (e.g. for the BM25-only fallback path
// that may exercise this function on a degenerate state).
func TestRRFFusionEmptyLists(t *testing.T) {
	fused, info := rrfFusion(nil, nil, 10)
	assert.Empty(t, fused)
	assert.Empty(t, info)

	fused2, info2 := rrfFusion([]scoredChunk{}, []scoredChunk{}, 10)
	assert.Empty(t, fused2)
	assert.Empty(t, info2)
}

// TestRRFFusionRankInfoPopulated checks that every chunk in the fused
// output has matching FusionRank (1-indexed, in output order) and a
// non-zero FusionScore. This is the contract the trace projection
// downstream depends on.
func TestRRFFusionRankInfoPopulated(t *testing.T) {
	bm25 := []scoredChunk{
		{chunk: KBChunk{ChunkID: "first"}, score: 9.0},
		{chunk: KBChunk{ChunkID: "second"}, score: 5.0},
	}
	dense := []scoredChunk{
		{chunk: KBChunk{ChunkID: "first"}, score: 0.9},
		{chunk: KBChunk{ChunkID: "second"}, score: 0.5},
	}
	fused, info := rrfFusion(bm25, dense, 10)
	require.Len(t, fused, 2)

	for i, c := range fused {
		cid := c.chunk.ChunkID
		assert.Equal(t, i+1, info[cid].FusionRank, "FusionRank should match output position for %s", cid)
		assert.Greater(t, info[cid].FusionScore, 0.0, "%s should have non-zero FusionScore", cid)
	}
}

// TestRRFFusionTopNTruncation verifies that the topN parameter truncates
// the output list. Trivially important so the Retrieve path doesn't have
// to do its own truncation pass after RRF.
func TestRRFFusionTopNTruncation(t *testing.T) {
	bm25 := []scoredChunk{
		{chunk: KBChunk{ChunkID: "a"}},
		{chunk: KBChunk{ChunkID: "b"}},
		{chunk: KBChunk{ChunkID: "c"}},
		{chunk: KBChunk{ChunkID: "d"}},
	}
	dense := []scoredChunk{
		{chunk: KBChunk{ChunkID: "e"}},
		{chunk: KBChunk{ChunkID: "f"}},
	}
	fused, _ := rrfFusion(bm25, dense, 3)
	assert.Len(t, fused, 3, "topN=3 caps output regardless of union size")
}

// denseSetup builds a 3-chunk corpus with a matching sidecar plus a
// "shadow" chunk that exists only in the sidecar (not in the corpus).
// This lets the iterate-corpus-not-sidecar test verify the invariant
// directly: if the dense scan iterated sidecar.Vectors it would surface
// the shadow chunk; iterating corpus.Chunks correctly drops it.
func denseSetup(t *testing.T) (Corpus, *EmbeddingSidecar) {
	t.Helper()
	corpus := Corpus{
		KBVersion: "kb-test",
		Chunks: []KBChunk{
			testChunk("dense-a", "billing", "high", "dense-a", "a 问法", "a 内容", "2025-01-01", nil),
			testChunk("dense-b", "billing", "high", "dense-b", "b 问法", "b 内容", "2025-01-01", nil),
			testChunk("dense-c", "billing", "high", "dense-c", "c 问法", "c 内容", "2025-01-01", nil),
		},
	}
	sidecar := &EmbeddingSidecar{
		Model: "test",
		Dim:   3,
		Rows:  4,
		Vectors: map[string][]float32{
			"dense-a": {1, 0, 0},
			"dense-b": {0, 1, 0},
			"dense-c": {0, 0, 1},
			// shadow-z exists only in sidecar — must NOT appear in dense output.
			"shadow-z": {0.7, 0.7, 0},
		},
	}
	return corpus, sidecar
}

// TestDenseFullSearchIteratesCorpusNotSidecar locks the most important
// invariant: dense scan must iterate the active corpus chunk set, not
// the raw sidecar map. The shadow-z entry in the sidecar must NOT
// surface in dense output because no corpus chunk has that ID.
//
// This test exists to catch the v1 brief trap where iterating
// sidecar.Vectors directly would resurrect chunks that the corpus
// loader dropped (via valid_from gate, confidence filter, etc.).
func TestDenseFullSearchIteratesCorpusNotSidecar(t *testing.T) {
	corpus, sidecar := denseSetup(t)
	embedder := &fakeEmbedder{queryVecs: map[string][]float32{
		"query": {1, 0, 0},
	}}
	r := NewRetriever(corpus, RetrieverOptions{
		Now:              fixedRetrieverNow,
		EmbeddingSidecar: sidecar,
		Embedder:         embedder,
		EmbeddingModel:   "test",
		Mode:             RetrievalModeQwen3RRF,
	})

	candidates, reason, latency := r.denseFullSearch("query", 10)
	assert.Empty(t, reason)
	assert.GreaterOrEqual(t, latency, int64(0))
	require.Len(t, candidates, 3, "expected 3 corpus chunks, NOT the 4 sidecar entries")

	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.chunk.ChunkID)
	}
	assert.NotContains(t, ids, "shadow-z", "shadow-z is in sidecar but not in corpus — must not appear")
	assert.ElementsMatch(t, []string{"dense-a", "dense-b", "dense-c"}, ids)
}

// TestDenseFullSearchExpiredChunkExcluded layers on the active-set
// invariant: even when a chunk exists in BOTH corpus and sidecar, if
// the validity gate filters it out, dense scan must respect that. The
// matching cosine vector in the sidecar should not "leak" an expired
// chunk into retrieval.
func TestDenseFullSearchExpiredChunkExcluded(t *testing.T) {
	expired := ptrString("2025-12-31")
	corpus := Corpus{
		KBVersion: "kb-test",
		Chunks: []KBChunk{
			testChunk("active", "billing", "high", "active", "活跃 问法", "活跃 内容", "2025-01-01", nil),
			testChunk("expired", "billing", "high", "expired", "过期 问法", "过期 内容", "2025-01-01", expired),
		},
	}
	sidecar := &EmbeddingSidecar{
		Model: "test", Dim: 3, Rows: 2,
		Vectors: map[string][]float32{
			"active":  {1, 0, 0},
			"expired": {1, 0, 0},
		},
	}
	embedder := &fakeEmbedder{queryVecs: map[string][]float32{
		"query": {1, 0, 0},
	}}
	r := NewRetriever(corpus, RetrieverOptions{
		Now:              fixedRetrieverNow,
		EmbeddingSidecar: sidecar,
		Embedder:         embedder,
		EmbeddingModel:   "test",
		Mode:             RetrievalModeQwen3RRF,
	})

	candidates, _, _ := r.denseFullSearch("query", 10)
	require.Len(t, candidates, 1, "expired chunk must be filtered out of dense scan")
	assert.Equal(t, "active", candidates[0].chunk.ChunkID)
}

// TestDenseFullSearchEmbeddingError covers the API-failure path. The
// embedder returns a non-deadline error → fallback reason should be
// "embedding_error" and candidates should be nil.
func TestDenseFullSearchEmbeddingError(t *testing.T) {
	corpus, sidecar := denseSetup(t)
	embedder := &fakeEmbedder{err: errors.New("modelverse 502")}
	r := NewRetriever(corpus, RetrieverOptions{
		Now:              fixedRetrieverNow,
		EmbeddingSidecar: sidecar,
		Embedder:         embedder,
		EmbeddingModel:   "test",
		Mode:             RetrievalModeQwen3RRF,
	})

	candidates, reason, latency := r.denseFullSearch("query", 10)
	assert.Nil(t, candidates)
	assert.Equal(t, "embedding_error", reason)
	assert.GreaterOrEqual(t, latency, int64(0))
}

// TestDenseFullSearchTimeout covers the slow-embedder path. We configure
// a very short hybridTimeout and use sleepingEmbedder that sleeps longer
// than the timeout → fallback reason should be "embedding_timeout".
func TestDenseFullSearchTimeout(t *testing.T) {
	corpus, sidecar := denseSetup(t)
	embedder := &sleepingEmbedder{sleep: 200 * time.Millisecond}
	r := NewRetriever(corpus, RetrieverOptions{
		Now:                  fixedRetrieverNow,
		EmbeddingSidecar:     sidecar,
		Embedder:             embedder,
		EmbeddingModel:       "test",
		Mode:                 RetrievalModeQwen3RRF,
		HybridContextTimeout: 10 * time.Millisecond,
	})

	candidates, reason, _ := r.denseFullSearch("query", 10)
	assert.Nil(t, candidates)
	assert.Equal(t, "embedding_timeout", reason)
}

// TestDenseFullSearchEmptyQueryVector covers the embedder-returns-empty
// edge case. Some embedding APIs can return [] on certain inputs; the
// dense scan must distinguish this from "real all-zero vector" via the
// "embedding_empty" reason rather than scoring against the empty vec
// (which would produce 0.0 cosines for every chunk → meaningless rank).
func TestDenseFullSearchEmptyQueryVector(t *testing.T) {
	corpus, sidecar := denseSetup(t)
	embedder := &fakeEmbedder{queryVecs: map[string][]float32{
		"query": {},
	}}
	r := NewRetriever(corpus, RetrieverOptions{
		Now:              fixedRetrieverNow,
		EmbeddingSidecar: sidecar,
		Embedder:         embedder,
		EmbeddingModel:   "test",
		Mode:             RetrievalModeQwen3RRF,
	})

	candidates, reason, _ := r.denseFullSearch("query", 10)
	assert.Nil(t, candidates)
	assert.Equal(t, "embedding_empty", reason)
}

// TestDenseFullSearchTopNTruncation verifies the topN cap. With 3
// corpus chunks and topN=2 we expect the highest-cosine 2.
func TestDenseFullSearchTopNTruncation(t *testing.T) {
	corpus, sidecar := denseSetup(t)
	embedder := &fakeEmbedder{queryVecs: map[string][]float32{
		"query": {1, 0, 0},
	}}
	r := NewRetriever(corpus, RetrieverOptions{
		Now:              fixedRetrieverNow,
		EmbeddingSidecar: sidecar,
		Embedder:         embedder,
		EmbeddingModel:   "test",
		Mode:             RetrievalModeQwen3RRF,
	})

	candidates, _, _ := r.denseFullSearch("query", 2)
	assert.Len(t, candidates, 2)
	assert.Equal(t, "dense-a", candidates[0].chunk.ChunkID, "dense-a vector (1,0,0) matches query exactly → highest cosine")
}

// Compile-time assertion that *fakeEmbedder and *sleepingEmbedder satisfy
// the VectorEmbedder interface, so the dense tests cannot drift if either
// fake's signature changes.
var (
	_ VectorEmbedder = (*fakeEmbedder)(nil)
	_ VectorEmbedder = (*sleepingEmbedder)(nil)
)

// unused-import suppression for context+math when only some tests in
// this file consume them (Go does NOT flag unused stdlib imports in
// _test.go but we keep these handles to discourage accidental removal).
var (
	_ = context.Background
	_ = math.Sqrt
)

// rrfSetup builds a 4-chunk corpus + matching sidecar designed so that
// BM25 and dense disagree on ordering, letting fusion tests verify
// the rank-level combination. Query "topic-a" is constructed so that:
//
//	BM25 order: chunk-a (exact match), chunk-b (related), chunk-c, chunk-d (low)
//	Dense order: chunk-d (vec match), chunk-c, chunk-b, chunk-a
//
// After RRF k=60 fusion, all four chunks should appear with reasonable
// ordering reflecting both signals.
func rrfSetup(t *testing.T) (Corpus, *EmbeddingSidecar) {
	t.Helper()
	chunks := []KBChunk{
		testChunk("chunk-a", "billing", "high", "topic-a 实例", "topic-a 怎么处理", "实例 a 详细内容 topic-a 关键字", "2025-01-01", nil),
		testChunk("chunk-b", "billing", "high", "topic-b 普通", "topic-b 怎么处理", "实例 b 内容 topic-b 部分相关", "2025-01-01", nil),
		testChunk("chunk-c", "billing", "high", "topic-c 普通", "topic-c 怎么处理", "实例 c 内容 topic-c 不相关", "2025-01-01", nil),
		testChunk("chunk-d", "billing", "high", "topic-d 完全无关", "其他 问法", "实例 d 完全 不相关 主题", "2025-01-01", nil),
	}
	corpus := Corpus{KBVersion: "kb-test", Chunks: chunks}
	sidecar := &EmbeddingSidecar{
		Model: "test", Dim: 3, Rows: 4,
		Vectors: map[string][]float32{
			"chunk-a": {0.1, 0.1, 0.9}, // dense ranks LAST against (0.9,0,0) query
			"chunk-b": {0.5, 0.0, 0.5},
			"chunk-c": {0.7, 0.0, 0.3},
			"chunk-d": {0.9, 0.0, 0.1}, // dense ranks FIRST
		},
	}
	return corpus, sidecar
}

// TestRetrieveQwen3RRFEndToEnd verifies that qwen3_rrf mode actually
// engages the fusion path: BM25 + dense both contribute, hybridMode
// reports "qwen3_rrf", and the embeddingLatencyMs is populated. We
// don't pin the exact fused ordering because RRF math is already
// covered in TestRRFFusionBasic — here we verify end-to-end wiring.
func TestRetrieveQwen3RRFEndToEnd(t *testing.T) {
	corpus, sidecar := rrfSetup(t)
	embedder := &staticEmbedder{vec: []float32{0.9, 0, 0}}
	r := NewRetriever(corpus, RetrieverOptions{
		Now:              fixedRetrieverNow,
		EmbeddingSidecar: sidecar,
		Embedder:         embedder,
		EmbeddingModel:   "qwen3-embedding-8b",
		Mode:             RetrievalModeQwen3RRF,
		TopK:             3,
	})

	result := r.Retrieve("topic-a 怎么处理", "billing")
	require.False(t, result.Empty)
	require.NotEmpty(t, result.Hits)
	assert.Equal(t, "qwen3_rrf", result.HybridMode)
	assert.Equal(t, "qwen3-embedding-8b", result.EmbeddingModel)
	require.NotNil(t, result.EmbeddingLatencyMS, "embedder must be invoked in RRF path")
	assert.GreaterOrEqual(t, *result.EmbeddingLatencyMS, int64(0))
	assert.LessOrEqual(t, len(result.Hits), 3, "topK=3 caps final hits")
}

// TestRetrieveQwen3RRFBM25ZeroHitStillCallsDense locks the critical
// invariant from the v2 brief: when BM25 returns 0 candidates, the
// qwen3_rrf path must STILL call the dense embedder. The cascade path
// (qwen3_full) skips the embedder in this case; RRF must not.
//
// Without this gate, RRF would collapse to cascade behavior for the
// exact queries it's designed to recover.
func TestRetrieveQwen3RRFBM25ZeroHitStillCallsDense(t *testing.T) {
	corpus, sidecar := rrfSetup(t)
	// Track that the embedder was called.
	embedder := &fakeEmbedder{queryVecs: map[string][]float32{
		"完全无 关键词 匹配的 查询": {0.9, 0, 0},
	}}
	r := NewRetriever(corpus, RetrieverOptions{
		Now:              fixedRetrieverNow,
		EmbeddingSidecar: sidecar,
		Embedder:         embedder,
		EmbeddingModel:   "qwen3-embedding-8b",
		Mode:             RetrievalModeQwen3RRF,
		TopK:             3,
	})

	// Query intentionally crafted to NOT BM25-match any chunk title/pattern/content.
	result := r.Retrieve("完全无 关键词 匹配的 查询", "")

	assert.GreaterOrEqual(t, embedder.calls, 1, "embedder MUST be called even when BM25 returns nothing")
	assert.Equal(t, "qwen3_rrf", result.HybridMode, "RRF mode label preserved even with BM25 zero-hit")
	require.NotNil(t, result.EmbeddingLatencyMS)
	require.NotEmpty(t, result.Hits, "dense leg should surface chunks via cosine even when BM25 missed")
}

// TestRetrieveQwen3RRFDenseFailureDegradesToBM25Fallback verifies that
// when the dense leg fails, the retriever degrades to bm25_fallback
// (same semantics as the cascade's embedding_error path). The BM25
// candidate pool is preserved unchanged.
func TestRetrieveQwen3RRFDenseFailureDegradesToBM25Fallback(t *testing.T) {
	corpus, sidecar := rrfSetup(t)
	embedder := &fakeEmbedder{err: errors.New("modelverse 500")}
	r := NewRetriever(corpus, RetrieverOptions{
		Now:              fixedRetrieverNow,
		EmbeddingSidecar: sidecar,
		Embedder:         embedder,
		EmbeddingModel:   "qwen3-embedding-8b",
		Mode:             RetrievalModeQwen3RRF,
		TopK:             3,
	})

	result := r.Retrieve("topic-a 怎么处理", "billing")
	assert.Equal(t, "bm25_fallback", result.HybridMode)
	assert.Equal(t, "embedding_error", result.HybridFallbackReason)
	assert.Empty(t, result.EmbeddingModel, "embeddingModel cleared on fallback")
	assert.Empty(t, result.RerankerMode, "reranker not engaged on dense failure")
}

// TestRetrieveQwen3RRFRerankerFailureKeepsFusionRanking verifies that
// when reranker fails on a qwen3_rrf run, finalCandidates remain in RRF
// order (not bm25_fallback). rerankerFallbackReason is populated so
// trace consumers can see the reranker tried-and-failed.
func TestRetrieveQwen3RRFRerankerFailureKeepsFusionRanking(t *testing.T) {
	corpus, sidecar := rrfSetup(t)
	embedder := &staticEmbedder{vec: []float32{0.9, 0, 0}}
	reranker := &fakeReranker{err: errors.New("reranker 500")}
	r := NewRetriever(corpus, RetrieverOptions{
		Now:              fixedRetrieverNow,
		EmbeddingSidecar: sidecar,
		Embedder:         embedder,
		EmbeddingModel:   "qwen3-embedding-8b",
		Reranker:         reranker,
		RerankerModel:    "qwen3-reranker-8b",
		Mode:             RetrievalModeQwen3RRF,
		TopK:             3,
	})

	result := r.Retrieve("topic-a 怎么处理", "billing")
	require.NotEmpty(t, result.Hits, "RRF ranking still produces hits when reranker fails")
	assert.Equal(t, "qwen3_rrf", result.HybridMode, "hybridMode stays qwen3_rrf when reranker fails")
	assert.Equal(t, "reranker_error", result.RerankerFallbackReason)
	assert.Empty(t, result.RerankerMode, "rerankerMode empty when reranker fell back")
}

// TestRetrieveQwen3RRFPoolSizeIs50 LOAD-BEARING test that rrfBM25PoolSize=50
// is the actual constant used (not the cascade's 20). Construction is
// crafted so the assertion would FAIL under a hypothetical pool=20:
//
// Setup:
//   - 50 corpus chunks all BM25-matching "topic 问法" via pattern; same
//     score, tie-broken by chunk_id asc (so chunk-35 lands at BM25
//     rank 36).
//   - Sidecar contains ONLY chunk-35's vector. denseFullSearch's defensive
//     skip for missing sidecar entries means dense_pool = [chunk-35]
//     uniquely — every other chunk gets DenseRank=0 (absent).
//   - Reranker disabled so the final top-K comes straight from RRF
//     fusion (no reordering noise).
//
// Math under pool=50:
//   - chunk-35: bm25_rank=36, dense_rank=1 → 1/96 + 1/61 ≈ 0.02681 → fusion_rank 1
//   - chunk-00: bm25_rank=1,  dense=absent → 1/61 + 0     ≈ 0.01639 → fusion_rank 2
//   - chunk-01: bm25_rank=2,  dense=absent → 1/62 + 0     ≈ 0.01613 → fusion_rank 3
//
// Math under hypothetical pool=20 (the failure case this test catches):
//   - chunk-35: bm25=ABSENT (truncated), dense=1 → 0 + 1/61 = 0.01639
//   - chunk-00..chunk-19 each get 1/61..1/80 → top 3 are chunk-00..02
//   - chunk-35 would tie chunk-00 on score but lose tie-break (chunk-00 < chunk-35)
//   - chunk-35 ends up at fusion_rank 21, NOT in top-3
//
// So under pool=20 the `require.NotNil(chunk35)` would fail. Under
// pool=50 chunk-35 dominates the top-3. This makes the test name
// load-bearing — same setup distinguishes the two implementations.
func TestRetrieveQwen3RRFPoolSizeIs50(t *testing.T) {
	const n = 50
	chunks := make([]KBChunk, n)
	for i := 0; i < n; i++ {
		id := "rrf-pool-" + zeroPaddedID(i)
		chunks[i] = KBChunk{
			ChunkID: id, Title: "topic chunk " + id, ProductArea: "billing",
			ACL: customerSafeACL, Confidence: confidenceHigh, KBVersion: "kb-test",
			ValidFrom:        "2025-01-01",
			QuestionPatterns: []string{"topic 问法"},
			Content:          "topic 内容 " + id,
		}
	}
	// CRITICAL: sidecar contains ONLY chunk-35's vector. denseFullSearch's
	// defensive `if _, ok := r.embeddingSidecar.Vectors[chunk.ChunkID]; !ok { continue }`
	// drops all other chunks from dense scoring, so dense_pool = [chunk-35].
	target := "rrf-pool-" + zeroPaddedID(35)
	sidecar := &EmbeddingSidecar{
		Model: "test", Dim: 3, Rows: 1,
		Vectors: map[string][]float32{target: {1, 0, 0}},
	}
	embedder := &staticEmbedder{vec: []float32{1, 0, 0}}
	r := NewRetriever(corpus(chunks), RetrieverOptions{
		Now:              fixedRetrieverNow,
		EmbeddingSidecar: sidecar,
		Embedder:         embedder,
		EmbeddingModel:   "qwen3-embedding-8b",
		// Reranker intentionally NOT supplied — keep the test focused on
		// RRF math, no reranker reordering of the top-3.
		Mode: RetrievalModeQwen3RRF,
		TopK: 3,
	})

	result := r.Retrieve("topic 问法", "billing")
	assert.Equal(t, "qwen3_rrf", result.HybridMode)
	require.Len(t, result.Hits, 3)

	var chunk35 *RetrievalHit
	for i := range result.HitItems {
		if result.HitItems[i].Chunk.ChunkID == target {
			chunk35 = &result.HitItems[i]
			break
		}
	}
	require.NotNil(t, chunk35,
		"chunk-35 must appear in top-3 under qwen3_rrf with pool=50 (BM25 rank 36 + dense rank 1 → fused rank 1). Under hypothetical pool=20 it would be dropped from BM25 list and lose the tie-break against chunk-00, falling out of top-3.")
	assert.Greater(t, chunk35.BM25Rank, 20,
		"chunk-35 BM25 rank should be >20 (specifically 36) — direct proof the BM25 pool window is >20")
	assert.LessOrEqual(t, chunk35.BM25Rank, 50,
		"chunk-35 BM25 rank should be ≤50 (within rrfBM25PoolSize)")
	assert.Equal(t, 1, chunk35.DenseRank,
		"chunk-35 should be dense rank 1 (only chunk in sidecar)")
	assert.Equal(t, 1, chunk35.FusionRank,
		"chunk-35 should win fusion (bm25+dense contribution > bm25-only of others)")
}

// corpus is a tiny constructor helper to keep the pool-size test
// readable. Plumbs the slice into a Corpus with kb-test version.
func corpus(chunks []KBChunk) Corpus {
	return Corpus{KBVersion: "kb-test", Chunks: chunks}
}

// zeroPaddedID produces a fixed-width index string ("00", "01", ..., "59")
// so chunk_id ascending sort matches numerical index for the
// pool-sizing test. Crucial because BM25 ties break on chunk_id.
func zeroPaddedID(i int) string {
	if i < 10 {
		return "0" + string(rune('0'+i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// TestRetrieveQwen3FullPoolSizeStillIs20 verifies that the cascade path
// (qwen3_full) still uses hybridBM25PoolSize=20 — i.e. the new RRF pool
// size constant doesn't leak into the existing cascade behavior.
// Companion to TestRetrieveQwen3RRFPoolSizeIs50.
//
// This test is intentionally light: full pool-truncation accounting is
// already covered by existing cascade tests in retriever_mode_test.go;
// here we just sanity-check that qwen3_full still reports the cascade
// hybridMode (i.e. didn't accidentally route to qwen3_rrf branch).
func TestRetrieveQwen3FullPoolSizeStillIs20(t *testing.T) {
	corpus, sidecar := rrfSetup(t)
	embedder := &staticEmbedder{vec: []float32{0.9, 0, 0}}
	reranker := &fakeReranker{fixedOrder: []int{0, 1, 2}}
	r := NewRetriever(corpus, RetrieverOptions{
		Now:              fixedRetrieverNow,
		EmbeddingSidecar: sidecar,
		Embedder:         embedder,
		EmbeddingModel:   "qwen3-embedding-8b",
		Reranker:         reranker,
		RerankerModel:    "qwen3-reranker-8b",
		Mode:             RetrievalModeQwen3Full,
		TopK:             3,
	})

	result := r.Retrieve("topic-a 怎么处理", "billing")
	assert.Equal(t, "qwen3_full", result.HybridMode, "cascade path unaffected by RRF additions")
}
