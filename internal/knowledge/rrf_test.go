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
