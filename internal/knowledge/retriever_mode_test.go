package knowledge

import (
	"context"
	"errors"
	"testing"
)

// fakeReranker returns a deterministic ranking for tests: results[i].Index
// reorders the input docs by the corresponding fixedOrder entry; if Err is
// non-nil the call errors out with that error (so tests can simulate
// timeout/error/empty paths without an HTTP server).
type fakeReranker struct {
	fixedOrder []int
	scores     []float64
	err        error
	empty      bool
}

func (f *fakeReranker) Rerank(_ context.Context, _ string, docs []string, _ int) ([]RerankerResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.empty {
		return nil, nil
	}
	results := make([]RerankerResult, 0, len(f.fixedOrder))
	for i, idx := range f.fixedOrder {
		if idx < 0 || idx >= len(docs) {
			continue
		}
		score := 1.0
		if i < len(f.scores) {
			score = f.scores[i]
		}
		results = append(results, RerankerResult{Index: idx, Score: score})
	}
	return results, nil
}

// staticEmbedder returns a fixed vector regardless of input. Combined with a
// sidecar whose chunk vectors we control, this lets us steer cosine ranking
// deterministically.
type staticEmbedder struct {
	vec []float32
	err error
}

func (f *staticEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.vec, nil
}

// buildCorpus3 returns a 3-chunk corpus and matching sidecar where cosine
// of the query vec against chunk vectors gives:
//
//	chunk-a: cosine 0.9 (BM25-matching content)
//	chunk-b: cosine 0.5
//	chunk-c: cosine 0.1
//
// All three chunks BM25-match the query "alpha beta gamma".
func buildCorpus3(t *testing.T) (Corpus, EmbeddingSidecar) {
	t.Helper()
	chunks := []KBChunk{
		{ChunkID: "a", Title: "alpha", Content: "alpha beta gamma A", Confidence: confidenceHigh,
			QuestionPatterns: []string{"alpha beta gamma"}},
		{ChunkID: "b", Title: "alpha", Content: "alpha beta gamma B", Confidence: confidenceHigh,
			QuestionPatterns: []string{"alpha beta gamma"}},
		{ChunkID: "c", Title: "alpha", Content: "alpha beta gamma C", Confidence: confidenceHigh,
			QuestionPatterns: []string{"alpha beta gamma"}},
	}
	corpus := Corpus{Chunks: chunks, KBVersion: "test"}
	sidecar := EmbeddingSidecar{
		Model: "test-model", Dim: 3, Rows: 3,
		Vectors: map[string][]float32{
			"a": {0.9, 0.1, 0.1},
			"b": {0.5, 0.5, 0.5},
			"c": {0.1, 0.9, 0.1},
		},
	}
	return corpus, sidecar
}

// TestRetrieveBM25OnlyMode: explicit bm25_only mode bypasses embedder even
// when one is provided. Trace records HybridMode="bm25_only" + no embedding
// fields. This is the new behavior gate for B.3 — callers can force-disable
// hybrid without removing the embedder/sidecar.
func TestRetrieveBM25OnlyMode(t *testing.T) {
	t.Parallel()
	corpus, sidecar := buildCorpus3(t)
	r := NewRetriever(corpus, RetrieverOptions{
		Mode:             RetrievalModeBM25Only,
		EmbeddingSidecar: &sidecar,
		Embedder:         &staticEmbedder{vec: []float32{1, 0, 0}},
		EmbeddingModel:   "test-model",
		TopK:             3,
		Threshold:        0.01,
	})
	got := r.Retrieve("alpha beta gamma", "")
	if got.HybridMode != "bm25_only" {
		t.Fatalf("HybridMode = %q, want bm25_only", got.HybridMode)
	}
	if got.EmbeddingModel != "" {
		t.Fatalf("EmbeddingModel = %q, want empty (embedder not invoked)", got.EmbeddingModel)
	}
	if got.EmbeddingLatencyMS != nil {
		t.Fatalf("EmbeddingLatencyMS = %v, want nil", got.EmbeddingLatencyMS)
	}
}

// TestRetrieveHybridCosineMode: cosine stage runs, but no reranker even if
// configured (reranker stage only fires for hybrid_rerank/qwen3_full).
func TestRetrieveHybridCosineMode(t *testing.T) {
	t.Parallel()
	corpus, sidecar := buildCorpus3(t)
	r := NewRetriever(corpus, RetrieverOptions{
		Mode:             RetrievalModeHybridCosine,
		EmbeddingSidecar: &sidecar,
		Embedder:         &staticEmbedder{vec: []float32{1, 0, 0}},
		EmbeddingModel:   "text-embedding-3-large",
		// Reranker is set but mode is hybrid_cosine — should be ignored.
		Reranker:      &fakeReranker{fixedOrder: []int{2, 1, 0}},
		RerankerModel: "qwen3-reranker-8b",
		TopK:          3,
		Threshold:     0.01,
	})
	got := r.Retrieve("alpha beta gamma", "")
	if got.HybridMode != "hybrid_cosine" {
		t.Fatalf("HybridMode = %q, want hybrid_cosine", got.HybridMode)
	}
	if got.EmbeddingModel != "text-embedding-3-large" {
		t.Fatalf("EmbeddingModel = %q, want text-embedding-3-large", got.EmbeddingModel)
	}
	if got.RerankerMode != "" {
		t.Fatalf("RerankerMode = %q, want empty (reranker stage not engaged in hybrid_cosine)", got.RerankerMode)
	}
	if got.RerankerLatencyMS != nil {
		t.Fatalf("RerankerLatencyMS = %v, want nil", got.RerankerLatencyMS)
	}
	// Cosine should rank chunk-a (vec 0.9,0.1,0.1 vs query 1,0,0) first.
	if len(got.Hits) < 1 || got.Hits[0].ChunkID != "a" {
		t.Fatalf("cosine top = %#v, want chunk a first", got.Hits)
	}
}

// TestRetrieveHybridRerankMode: cosine ranks {a,b,c}; reranker overrides to
// {c,b,a}. Trace records hybrid_rerank, RerankerMode=model, both latencies
// populated, no fallback reason.
func TestRetrieveHybridRerankMode(t *testing.T) {
	t.Parallel()
	corpus, sidecar := buildCorpus3(t)
	r := NewRetriever(corpus, RetrieverOptions{
		Mode:             RetrievalModeHybridRerank,
		EmbeddingSidecar: &sidecar,
		Embedder:         &staticEmbedder{vec: []float32{1, 0, 0}},
		EmbeddingModel:   "text-embedding-3-large",
		// Cosine pool will be [a, b, c]; reranker reorders to [c, b, a]
		// by returning indices [2, 1, 0].
		Reranker: &fakeReranker{
			fixedOrder: []int{2, 1, 0},
			scores:     []float64{0.95, 0.5, 0.1},
		},
		RerankerModel: "qwen3-reranker-8b",
		TopK:          3,
		Threshold:     0.01,
	})
	got := r.Retrieve("alpha beta gamma", "")
	if got.HybridMode != "hybrid_rerank" {
		t.Fatalf("HybridMode = %q, want hybrid_rerank", got.HybridMode)
	}
	if got.EmbeddingModel != "text-embedding-3-large" {
		t.Fatalf("EmbeddingModel = %q, want text-embedding-3-large", got.EmbeddingModel)
	}
	if got.RerankerMode != "qwen3-reranker-8b" {
		t.Fatalf("RerankerMode = %q, want qwen3-reranker-8b", got.RerankerMode)
	}
	if got.RerankerFallbackReason != "" {
		t.Fatalf("RerankerFallbackReason = %q, want empty", got.RerankerFallbackReason)
	}
	if got.RerankerLatencyMS == nil {
		t.Fatal("RerankerLatencyMS = nil, want populated")
	}
	if got.Hits[0].ChunkID != "c" {
		t.Fatalf("reranker top = %s, want c (overrode cosine)", got.Hits[0].ChunkID)
	}
}

// TestRetrieveQwen3FullMode: same flow as hybrid_rerank but the mode label
// is qwen3_full (signals downstream that qwen3-emb-8b sidecar is in use).
func TestRetrieveQwen3FullMode(t *testing.T) {
	t.Parallel()
	corpus, sidecar := buildCorpus3(t)
	r := NewRetriever(corpus, RetrieverOptions{
		Mode:             RetrievalModeQwen3Full,
		EmbeddingSidecar: &sidecar,
		Embedder:         &staticEmbedder{vec: []float32{1, 0, 0}},
		EmbeddingModel:   "qwen3-embedding-8b",
		Reranker: &fakeReranker{
			fixedOrder: []int{1, 0},
			scores:     []float64{0.9, 0.4},
		},
		RerankerModel: "qwen3-reranker-8b",
		TopK:          2,
		Threshold:     0.01,
	})
	got := r.Retrieve("alpha beta gamma", "")
	if got.HybridMode != "qwen3_full" {
		t.Fatalf("HybridMode = %q, want qwen3_full", got.HybridMode)
	}
	if got.EmbeddingModel != "qwen3-embedding-8b" {
		t.Fatalf("EmbeddingModel = %q, want qwen3-embedding-8b", got.EmbeddingModel)
	}
}

// TestRetrieveRerankerTimeoutFallsBackToCosine: reranker times out → cosine
// top-K returned, HybridMode stays hybrid_cosine, RerankerFallbackReason
// = reranker_timeout. Critical for the contract: reranker failure does NOT
// drop us back to BM25; we still have valid cosine signal.
func TestRetrieveRerankerTimeoutFallsBackToCosine(t *testing.T) {
	t.Parallel()
	corpus, sidecar := buildCorpus3(t)
	r := NewRetriever(corpus, RetrieverOptions{
		Mode:             RetrievalModeHybridRerank,
		EmbeddingSidecar: &sidecar,
		Embedder:         &staticEmbedder{vec: []float32{1, 0, 0}},
		EmbeddingModel:   "text-embedding-3-large",
		Reranker:         &fakeReranker{err: context.DeadlineExceeded},
		RerankerModel:    "qwen3-reranker-8b",
		TopK:             3,
		Threshold:        0.01,
	})
	got := r.Retrieve("alpha beta gamma", "")
	if got.HybridMode != "hybrid_cosine" {
		t.Fatalf("HybridMode = %q, want hybrid_cosine (reranker failed, cosine kept)", got.HybridMode)
	}
	if got.RerankerFallbackReason != "reranker_timeout" {
		t.Fatalf("RerankerFallbackReason = %q, want reranker_timeout", got.RerankerFallbackReason)
	}
	if got.RerankerMode != "" {
		t.Fatalf("RerankerMode = %q, want empty (reranker fell back)", got.RerankerMode)
	}
	if got.EmbeddingModel != "text-embedding-3-large" {
		t.Fatalf("EmbeddingModel = %q, want text-embedding-3-large (cosine still ran)", got.EmbeddingModel)
	}
	// Cosine ranking preserved: chunk-a first.
	if got.Hits[0].ChunkID != "a" {
		t.Fatalf("top after reranker fallback = %s, want a (cosine top)", got.Hits[0].ChunkID)
	}
}

// TestRetrieveRerankerErrorFallsBackToCosine: non-timeout error path. Reason
// must be "reranker_error", not "reranker_timeout".
func TestRetrieveRerankerErrorFallsBackToCosine(t *testing.T) {
	t.Parallel()
	corpus, sidecar := buildCorpus3(t)
	r := NewRetriever(corpus, RetrieverOptions{
		Mode:             RetrievalModeHybridRerank,
		EmbeddingSidecar: &sidecar,
		Embedder:         &staticEmbedder{vec: []float32{1, 0, 0}},
		EmbeddingModel:   "text-embedding-3-large",
		Reranker:         &fakeReranker{err: errors.New("HTTP 500")},
		RerankerModel:    "qwen3-reranker-8b",
		TopK:             3,
		Threshold:        0.01,
	})
	got := r.Retrieve("alpha beta gamma", "")
	if got.RerankerFallbackReason != "reranker_error" {
		t.Fatalf("RerankerFallbackReason = %q, want reranker_error", got.RerankerFallbackReason)
	}
}

// TestRetrieveRerankerEmptyFallsBackToCosine: server returns no results
// (parsed as nil by reranker package). Reason = "reranker_empty", cosine
// ranking preserved.
func TestRetrieveRerankerEmptyFallsBackToCosine(t *testing.T) {
	t.Parallel()
	corpus, sidecar := buildCorpus3(t)
	r := NewRetriever(corpus, RetrieverOptions{
		Mode:             RetrievalModeHybridRerank,
		EmbeddingSidecar: &sidecar,
		Embedder:         &staticEmbedder{vec: []float32{1, 0, 0}},
		EmbeddingModel:   "text-embedding-3-large",
		Reranker:         &fakeReranker{empty: true},
		RerankerModel:    "qwen3-reranker-8b",
		TopK:             3,
		Threshold:        0.01,
	})
	got := r.Retrieve("alpha beta gamma", "")
	if got.RerankerFallbackReason != "reranker_empty" {
		t.Fatalf("RerankerFallbackReason = %q, want reranker_empty", got.RerankerFallbackReason)
	}
}

// TestRetrieveDefaultModeWithoutEmbedder: empty Mode + no embedder/sidecar
// falls back to bm25_only — preserves all pre-B.3 call sites.
func TestRetrieveDefaultModeWithoutEmbedder(t *testing.T) {
	t.Parallel()
	corpus, _ := buildCorpus3(t)
	r := NewRetriever(corpus, RetrieverOptions{TopK: 3, Threshold: 0.01})
	got := r.Retrieve("alpha beta gamma", "")
	if got.HybridMode != "bm25_only" {
		t.Fatalf("HybridMode = %q, want bm25_only (back-compat)", got.HybridMode)
	}
}

// TestRetrieveDefaultModeWithEmbedderOnly: empty Mode + embedder+sidecar
// falls back to hybrid_cosine — preserves the pre-B.3 hybrid behavior for
// callers that haven't set Mode explicitly.
func TestRetrieveDefaultModeWithEmbedderOnly(t *testing.T) {
	t.Parallel()
	corpus, sidecar := buildCorpus3(t)
	r := NewRetriever(corpus, RetrieverOptions{
		EmbeddingSidecar: &sidecar,
		Embedder:         &staticEmbedder{vec: []float32{1, 0, 0}},
		EmbeddingModel:   "text-embedding-3-large",
		TopK:             3,
		Threshold:        0.01,
	})
	got := r.Retrieve("alpha beta gamma", "")
	if got.HybridMode != "hybrid_cosine" {
		t.Fatalf("HybridMode = %q, want hybrid_cosine (back-compat)", got.HybridMode)
	}
}
