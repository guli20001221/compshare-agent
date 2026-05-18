package knowledge

import (
	"context"
	"errors"
	"testing"
	"time"

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

// sleepingEmbedder simulates a slow embedding API. It honors ctx
// cancellation so retriever.hybridTimeout can fire (the actual
// EmbeddingLatencyMS measured will be ≈ hybridTimeout, not sleep).
type sleepingEmbedder struct {
	sleep time.Duration
	calls int
}

func (s *sleepingEmbedder) Embed(ctx context.Context, _ string) ([]float32, error) {
	s.calls++
	select {
	case <-time.After(s.sleep):
		return []float32{1, 0, 0}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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
	assert.Equal(t, "hybrid_cosine", got.HybridMode, "successful hybrid rerank should report HybridMode=hybrid_cosine")
	assert.Equal(t, "", got.HybridFallbackReason, "no fallback reason on successful rerank")
	require.NotNil(t, got.EmbeddingLatencyMS, "successful rerank invoked embedder, EmbeddingLatencyMS must be non-nil")
	assert.GreaterOrEqual(t, *got.EmbeddingLatencyMS, int64(0), "latency must be non-negative")
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
	assert.Equal(t, "bm25_fallback", got.HybridMode, "embedder error must surface as HybridMode=bm25_fallback")
	assert.Equal(t, "embedding_error", got.HybridFallbackReason, "generic non-timeout error should classify as embedding_error")
	require.NotNil(t, got.EmbeddingLatencyMS, "embedder was invoked, latency must be recorded even on error")
	assert.GreaterOrEqual(t, *got.EmbeddingLatencyMS, int64(0))
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
	// Configured path is hybrid even though BM25 returned nothing; reporting
	// bm25_only here would mislead ops dashboards into thinking the retriever
	// was misconfigured. The empty-result signal is carried by Empty=true.
	assert.Equal(t, "hybrid_cosine", got.HybridMode, "empty BM25 pool under hybrid configuration still reports HybridMode=hybrid_cosine")
	assert.Equal(t, "", got.HybridFallbackReason)
	// Embedder was never invoked → latency must be nil (distinguishing
	// "skipped" from a real 0ms round-trip is the whole point of *int64).
	assert.Nil(t, got.EmbeddingLatencyMS, "empty BM25 pool means embedder not invoked, latency must be nil")
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
	assert.Equal(t, "bm25_only", want.HybridMode, "no sidecar + no embedder should report HybridMode=bm25_only")
	assert.Equal(t, "", want.HybridFallbackReason)
	assert.Nil(t, want.EmbeddingLatencyMS, "bm25_only never invoked embedder, latency must be nil")
	for _, name := range []string{"only-sidecar", "only-embedder"} {
		t.Run(name, func(t *testing.T) {
			var got RetrievalResult
			if name == "only-sidecar" {
				got = rOnlySidecar.Retrieve(q, "billing")
			} else {
				got = rOnlyEmbedder.Retrieve(q, "billing")
			}
			assert.Equal(t, chunkIDs(want.Hits), chunkIDs(got.Hits), "BM25-only path should be identical when only one of sidecar/embedder is set")
			assert.Equal(t, "bm25_only", got.HybridMode, "exactly one of sidecar/embedder set still means hybrid is disabled")
			assert.Equal(t, "", got.HybridFallbackReason)
			assert.Nil(t, got.EmbeddingLatencyMS)
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
	assert.Equal(t, "bm25_fallback", got.HybridMode)
	assert.Equal(t, "embedding_empty", got.HybridFallbackReason)
	require.NotNil(t, got.EmbeddingLatencyMS, "embedder was invoked, latency recorded even when vec is empty")
	assert.GreaterOrEqual(t, *got.EmbeddingLatencyMS, int64(0))
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

// Explicit context.DeadlineExceeded must classify as embedding_timeout (not
// the generic embedding_error). This is the only case where ops needs to
// distinguish "model API was slow" from "model API returned an error" —
// it determines whether a hybridTimeout knob tweak would fix the situation
// or whether the upstream model is genuinely failing.
func TestRetrieverHybridContextDeadlineSetsTimeoutReason(t *testing.T) {
	corpus, sidecar := hybridSetup(t)
	embedder := &fakeEmbedder{err: context.DeadlineExceeded}
	r := NewRetriever(corpus, RetrieverOptions{
		Now:              fixedRetrieverNow,
		EmbeddingSidecar: sidecar,
		Embedder:         embedder,
	})
	got := r.Retrieve("a 主题问法", "billing")
	require.NotEmpty(t, got.Hits, "must still return BM25 top-K on timeout")
	assert.Equal(t, "bm25_fallback", got.HybridMode)
	assert.Equal(t, "embedding_timeout", got.HybridFallbackReason,
		"context.DeadlineExceeded must classify as embedding_timeout, not embedding_error")
}

// Real ctx-cancel timeout: hybridTimeout fires before sleepingEmbedder
// returns. EmbeddingLatencyMS must approximate the configured timeout
// (the ctx-cancel cost), giving ops a knob-tweak signal. We assert a
// generous range to avoid CI scheduler flake — the key invariant is
// that latency is meaningful (not 0, not absurdly large), not exact.
func TestRetrieverHybridLatencyApproximatesTimeoutOnRealCancel(t *testing.T) {
	corpus, sidecar := hybridSetup(t)
	embedder := &sleepingEmbedder{sleep: 500 * time.Millisecond}
	r := NewRetriever(corpus, RetrieverOptions{
		Now:                  fixedRetrieverNow,
		EmbeddingSidecar:     sidecar,
		Embedder:             embedder,
		HybridContextTimeout: 50 * time.Millisecond,
	})
	got := r.Retrieve("a 主题问法", "billing")
	assert.Equal(t, "bm25_fallback", got.HybridMode)
	assert.Equal(t, "embedding_timeout", got.HybridFallbackReason)
	require.NotNil(t, got.EmbeddingLatencyMS)
	// Latency should be roughly the timeout (50ms) — at least 40ms,
	// at most 250ms (CI scheduler / context cancel propagation slack).
	assert.GreaterOrEqual(t, *got.EmbeddingLatencyMS, int64(40),
		"latency should approximate the configured timeout, got %dms", *got.EmbeddingLatencyMS)
	assert.LessOrEqual(t, *got.EmbeddingLatencyMS, int64(250),
		"latency should not significantly exceed the timeout, got %dms", *got.EmbeddingLatencyMS)
}

// Wrapped context.DeadlineExceeded must still classify as embedding_timeout
// via errors.Is. Production embedder clients commonly wrap the underlying
// transport error with an annotation like "modelverse: %w".
func TestRetrieverHybridWrappedDeadlineSetsTimeoutReason(t *testing.T) {
	corpus, sidecar := hybridSetup(t)
	embedder := &fakeEmbedder{err: errors.Join(errors.New("modelverse client"), context.DeadlineExceeded)}
	r := NewRetriever(corpus, RetrieverOptions{
		Now:              fixedRetrieverNow,
		EmbeddingSidecar: sidecar,
		Embedder:         embedder,
	})
	got := r.Retrieve("a 主题问法", "billing")
	require.NotEmpty(t, got.Hits)
	assert.Equal(t, "bm25_fallback", got.HybridMode)
	assert.Equal(t, "embedding_timeout", got.HybridFallbackReason,
		"errors.Is unwrap must still detect DeadlineExceeded inside a wrapped error chain")
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
