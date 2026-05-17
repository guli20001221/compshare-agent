package knowledge

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeEmbedder returns a fixed vector per (question -> chunk_id-keyed map)
// or surfaces an error from the configured failure list.
type fakeEmbedder struct {
	queryVecs map[string][]float32
	err       error
	calls     int
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	vec, ok := f.queryVecs[text]
	if !ok {
		return []float32{0, 0, 0}, nil
	}
	return vec, nil
}

func hybridSetup(t *testing.T) (Corpus, *EmbeddingSidecar) {
	t.Helper()
	chunks := []KBChunk{
		testChunk("hybrid-a", "billing", "high", "billing topic A",
			"a 主题问法", "a 内容", "2025-01-01", nil),
		testChunk("hybrid-b", "billing", "high", "billing topic B",
			"b 主题问法", "b 内容", "2025-01-01", nil),
		testChunk("hybrid-c", "billing", "high", "billing topic C",
			"c 主题问法", "c 内容", "2025-01-01", nil),
	}
	corpus := Corpus{KBVersion: "kb-test", Chunks: chunks}
	sidecar := &EmbeddingSidecar{
		Model: "test",
		Dim:   3,
		Rows:  3,
		Vectors: map[string][]float32{
			// Embed query so that 'c' has the highest cosine, then 'b', then 'a'.
			// This is the inverse of the BM25 ranking (which prefers chunks that
			// share more characters with the query — see assertion below).
			"hybrid-a": {1, 0, 0},
			"hybrid-b": {0, 1, 0},
			"hybrid-c": {0, 0, 1},
		},
	}
	return corpus, sidecar
}

// Hybrid path: embedding rerank reverses the BM25 order when the cosine
// signal disagrees. This is the canonical "embedding helps" case.
func TestRetrieverHybridReordersByEmbeddingScore(t *testing.T) {
	corpus, sidecar := hybridSetup(t)
	embedder := &fakeEmbedder{
		queryVecs: map[string][]float32{
			// Aligns with hybrid-c's vector {0,0,1}.
			"a 主题问法": {0, 0, 1},
		},
	}
	r := NewRetriever(corpus, RetrieverOptions{
		Now:              fixedRetrieverNow,
		EmbeddingSidecar: sidecar,
		Embedder:         embedder,
	})
	got := r.Retrieve("a 主题问法", "billing")
	require.Len(t, got.Hits, 3, "expected all 3 chunks back as the BM25 pool size is 20")
	assert.Equal(t, "hybrid-c", got.Hits[0].ChunkID, "embedding rerank should put hybrid-c first because its vector aligns with the query")
	assert.Equal(t, 1, embedder.calls, "exactly one embedding call per query")
	// Scores carried back to the trace should be cosine (in [-1,1]), not BM25 (which would be > 0.5).
	assert.LessOrEqual(t, got.HitItems[0].Score, 1.0)
	assert.GreaterOrEqual(t, got.HitItems[0].Score, -1.0)
}

// Embedder error must fall back to BM25 top-K without panicking. The
// returned chunks come from the BM25-sorted candidate pool (truncated to topK).
func TestRetrieverHybridFallsBackToBM25OnEmbedError(t *testing.T) {
	corpus, sidecar := hybridSetup(t)
	embedder := &fakeEmbedder{err: errors.New("modelverse: timeout")}
	r := NewRetriever(corpus, RetrieverOptions{
		Now:              fixedRetrieverNow,
		EmbeddingSidecar: sidecar,
		Embedder:         embedder,
	})
	got := r.Retrieve("a 主题问法", "billing")
	require.NotEmpty(t, got.Hits)
	assert.Equal(t, "hybrid-a", got.Hits[0].ChunkID, "BM25 should rank hybrid-a first because the query contains 'a 主题问法'")
	assert.Equal(t, 1, embedder.calls, "fallback path still invoked embedder exactly once before deciding to fall back")
}

// When BM25 returns zero candidates, the retriever must not invoke the
// embedder (saving a network call) and must return an empty result.
func TestRetrieverHybridSkipsEmbedderOnEmptyBM25Pool(t *testing.T) {
	corpus, sidecar := hybridSetup(t)
	embedder := &fakeEmbedder{}
	r := NewRetriever(corpus, RetrieverOptions{
		Now:              fixedRetrieverNow,
		EmbeddingSidecar: sidecar,
		Embedder:         embedder,
	})
	got := r.Retrieve("zzzzzzzzzz qqqqqq", "billing")
	assert.True(t, got.Empty)
	assert.Equal(t, 0, embedder.calls, "no embedding call when BM25 pool is empty")
}

// Without an embedder or sidecar, the retriever takes the BM25-only path —
// preserving all behavior the existing retriever_test.go suite asserts.
func TestRetrieverHybridOptOutPreservesBM25Behavior(t *testing.T) {
	corpus, sidecar := hybridSetup(t)
	embedder := &fakeEmbedder{}

	rBoth := NewRetriever(corpus, RetrieverOptions{Now: fixedRetrieverNow})
	rOnlySidecar := NewRetriever(corpus, RetrieverOptions{Now: fixedRetrieverNow, EmbeddingSidecar: sidecar})
	rOnlyEmbedder := NewRetriever(corpus, RetrieverOptions{Now: fixedRetrieverNow, Embedder: embedder})

	q := "a 主题问法"
	want := rBoth.Retrieve(q, "billing")
	for _, name := range []string{"only-sidecar", "only-embedder"} {
		t.Run(name, func(t *testing.T) {
			var got RetrievalResult
			if name == "only-sidecar" {
				got = rOnlySidecar.Retrieve(q, "billing")
			} else {
				got = rOnlyEmbedder.Retrieve(q, "billing")
			}
			assert.Equal(t, chunkIDs(want.Hits), chunkIDs(got.Hits), "BM25-only path should be identical when only one of sidecar/embedder is set")
		})
	}
	assert.Equal(t, 0, embedder.calls, "embedder must not be invoked when sidecar is nil")
}

// Empty query embedding response (defensive — should never happen in
// production because ModelVerse always returns a 3072-dim vector on 200,
// but the client may surface a zero-length slice if the API contract drifts).
func TestRetrieverHybridFallsBackOnEmptyQueryEmbedding(t *testing.T) {
	corpus, sidecar := hybridSetup(t)
	embedder := &fakeEmbedder{queryVecs: map[string][]float32{"a 主题问法": {}}}
	r := NewRetriever(corpus, RetrieverOptions{
		Now:              fixedRetrieverNow,
		EmbeddingSidecar: sidecar,
		Embedder:         embedder,
	})
	got := r.Retrieve("a 主题问法", "billing")
	require.NotEmpty(t, got.Hits)
	assert.Equal(t, "hybrid-a", got.Hits[0].ChunkID, "empty embedding should fall back to BM25 top-3 ordering")
}

// Sidecar missing a chunk_id mid-rerank must drop that single candidate
// rather than crash. LoadPinnedCorpusWithEmbeddings's bijection guarantee
// should make this unreachable in production, but defensive code is still
// tested per brief §B.5 step 6 + reviewer area 3(d).
func TestRetrieverHybridDropsCandidateWithMissingSidecarVector(t *testing.T) {
	corpus, sidecar := hybridSetup(t)
	// Delete one vector from sidecar so the rerank loop hits the "ok=false"
	// branch in retriever.go (logged warning + skip).
	delete(sidecar.Vectors, "hybrid-b")
	embedder := &fakeEmbedder{queryVecs: map[string][]float32{
		"a 主题问法": {1, 0, 0}, // aligns with hybrid-a
	}}
	r := NewRetriever(corpus, RetrieverOptions{
		Now:              fixedRetrieverNow,
		EmbeddingSidecar: sidecar,
		Embedder:         embedder,
	})
	got := r.Retrieve("a 主题问法", "billing")
	// hybrid-b must be dropped from results; hybrid-a + hybrid-c survive.
	ids := chunkIDs(got.Hits)
	for _, id := range ids {
		if id == "hybrid-b" {
			t.Fatalf("chunk with missing sidecar vector should be dropped: got %v", ids)
		}
	}
	if len(ids) == 0 {
		t.Fatal("expected at least one hit even after dropping hybrid-b")
	}
}

// cosineSimilarity in retriever.go must give the same numeric result as
// embedding.Cosine. Light spot check (the parity contract is enforced by
// the 377-Q hybrid eval, which exercises the full BM25-pool + rerank path).
func TestRetrieverCosineSimilarityKnownValues(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{0, 1, 0}
	assert.InDelta(t, 0.0, cosineSimilarity(a, b), 1e-12)
	assert.InDelta(t, 1.0, cosineSimilarity(a, []float32{1, 0, 0}), 1e-12)
	assert.InDelta(t, -1.0, cosineSimilarity(a, []float32{-1, 0, 0}), 1e-12)
	assert.InDelta(t, 0.0, cosineSimilarity(a, []float32{}), 1e-12)
	assert.InDelta(t, 0.0, cosineSimilarity([]float32{}, b), 1e-12)
}
