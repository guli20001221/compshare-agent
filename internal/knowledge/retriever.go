package knowledge

import (
	"context"
	"log"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	defaultRetrieverTopK      = 3
	defaultRetrieverThreshold = 0.5
	hybridBM25PoolSize        = 20
)

// VectorEmbedder is satisfied by *internal/embedding.Client and by test
// doubles. Keeping it as a local interface lets the retriever stay free of
// the embedding package import dependency.
type VectorEmbedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

var beijingLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

type RetrieverOptions struct {
	TopK      int
	Threshold float64
	Now       func() time.Time
	// Hybrid retrieval is enabled when both EmbeddingSidecar and Embedder are
	// supplied. When either is nil, the retriever falls back to the existing
	// BM25-only path and all original behavior is preserved (including all
	// retriever_test.go expectations).
	EmbeddingSidecar *EmbeddingSidecar
	Embedder         VectorEmbedder
	// HybridContextTimeout bounds each query embedding call. Defaults to 1s
	// when zero. The retriever swallows embedding errors and falls back to
	// BM25 top-3 from its top-20 pool — see retrieveHybrid for the contract.
	HybridContextTimeout time.Duration
}

type RetrievalResult struct {
	Enabled         bool
	KBVersion       string
	QueryNormalized string
	Hits            []KBChunk
	HitItems        []RetrievalHit
	Empty           bool
}

type RetrievalHit struct {
	Chunk KBChunk
	Score float64
	Kept  bool
}

type Retriever struct {
	corpus           Corpus
	topK             int
	threshold        float64
	now              func() time.Time
	bm25             retrievalBM25Index
	embeddingSidecar *EmbeddingSidecar
	embedder         VectorEmbedder
	hybridTimeout    time.Duration
}

func NewRetriever(corpus Corpus, opts RetrieverOptions) *Retriever {
	topK := opts.TopK
	if topK <= 0 {
		topK = defaultRetrieverTopK
	}
	threshold := opts.Threshold
	if threshold <= 0 {
		threshold = defaultRetrieverThreshold
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	hybridTimeout := opts.HybridContextTimeout
	if hybridTimeout <= 0 {
		hybridTimeout = time.Second
	}
	return &Retriever{
		corpus:           corpus,
		topK:             topK,
		threshold:        threshold,
		now:              now,
		bm25:             newRetrievalBM25Index(corpus.Chunks),
		embeddingSidecar: opts.EmbeddingSidecar,
		embedder:         opts.Embedder,
		hybridTimeout:    hybridTimeout,
	}
}

// hybridEnabled reports whether the retriever should take the BM25-top-20
// then embedding-rerank path.
func (r *Retriever) hybridEnabled() bool {
	return r.embeddingSidecar != nil && r.embedder != nil
}

func (r *Retriever) Retrieve(question, productArea string) RetrievalResult {
	queryNormalized := NormalizeQuery(question)
	productArea = strings.TrimSpace(strings.ToLower(productArea))

	bm25Candidates := r.collectBM25Candidates(question, productArea)
	limit := r.topK
	if r.hybridEnabled() {
		limit = hybridBM25PoolSize
	}
	if len(bm25Candidates) > limit {
		bm25Candidates = bm25Candidates[:limit]
	}

	finalCandidates := bm25Candidates
	if r.hybridEnabled() && len(bm25Candidates) > 0 {
		finalCandidates = r.rerankByEmbedding(question, bm25Candidates)
	}

	if len(finalCandidates) > r.topK {
		finalCandidates = finalCandidates[:r.topK]
	}

	hits := make([]KBChunk, 0, len(finalCandidates))
	hitItems := make([]RetrievalHit, 0, len(finalCandidates))
	for _, candidate := range finalCandidates {
		hits = append(hits, candidate.chunk)
		hitItems = append(hitItems, RetrievalHit{
			Chunk: candidate.chunk,
			Score: candidate.score,
			Kept:  true,
		})
	}
	return RetrievalResult{
		Enabled:         true,
		KBVersion:       r.corpus.KBVersion,
		QueryNormalized: queryNormalized,
		Hits:            hits,
		HitItems:        hitItems,
		Empty:           len(hits) == 0,
	}
}

// collectBM25Candidates runs the existing BM25 scoring + filter + canonical sort.
// Returns *all* surviving candidates (caller truncates to topK or pool size).
func (r *Retriever) collectBM25Candidates(question, productArea string) []scoredChunk {
	queryTokens := tokenizeRetrievalText(question)
	candidates := make([]scoredChunk, 0, len(r.corpus.Chunks))
	for index, chunk := range r.corpus.Chunks {
		if !chunkActiveAt(chunk, r.now()) || chunk.Confidence == confidenceLow {
			continue
		}
		score := r.scoreChunk(queryTokens, productArea, index, chunk)
		if score < r.threshold {
			continue
		}
		candidates = append(candidates, scoredChunk{chunk: chunk, score: score})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.score != right.score {
			return left.score > right.score
		}
		if confidenceRank(left.chunk.Confidence) != confidenceRank(right.chunk.Confidence) {
			return confidenceRank(left.chunk.Confidence) > confidenceRank(right.chunk.Confidence)
		}
		return left.chunk.ChunkID < right.chunk.ChunkID
	})
	return candidates
}

// rerankByEmbedding takes a BM25 candidate pool (already sorted by BM25
// score) and reorders it by query-embedding cosine similarity against the
// pinned corpus sidecar. The returned candidates carry the *cosine* score
// in scoredChunk.score so trace records reflect the actual ranking signal.
//
// If the embedding call fails the function logs a structured warning and
// returns the BM25 pool unchanged (caller will then take top-K from that
// pool). Per user 2026-05-17 spec: "embedding 失败时降级 BM25".
//
// The Python eval pipeline (scripts/rag_w0/evaluate_retrieval.py --mode
// hybrid) must produce the same final top-K chunk_id sets on the same
// inputs for the 377-Q parity contract to hold.
func (r *Retriever) rerankByEmbedding(question string, bm25Pool []scoredChunk) []scoredChunk {
	ctx, cancel := context.WithTimeout(context.Background(), r.hybridTimeout)
	defer cancel()
	queryVec, err := r.embedder.Embed(ctx, question)
	if err != nil {
		log.Printf("rag.hybrid: query embedding failed, falling back to BM25 top-%d: %v", r.topK, err)
		return bm25Pool
	}
	if len(queryVec) == 0 {
		log.Printf("rag.hybrid: query embedding empty, falling back to BM25 top-%d", r.topK)
		return bm25Pool
	}
	reranked := make([]scoredChunk, 0, len(bm25Pool))
	for _, c := range bm25Pool {
		chunkVec, ok := r.embeddingSidecar.Vectors[c.chunk.ChunkID]
		if !ok {
			// Sidecar/corpus drift past LoadPinnedCorpusWithEmbeddings's bijection
			// guarantee should be impossible; log and skip this candidate so the
			// hybrid path stays safe even when the invariant is somehow violated.
			log.Printf("rag.hybrid: sidecar missing vector for chunk %q, dropping from rerank", c.chunk.ChunkID)
			continue
		}
		reranked = append(reranked, scoredChunk{
			chunk: c.chunk,
			score: cosineSimilarity(queryVec, chunkVec),
		})
	}
	sort.SliceStable(reranked, func(i, j int) bool {
		left, right := reranked[i], reranked[j]
		if left.score != right.score {
			return left.score > right.score
		}
		if confidenceRank(left.chunk.Confidence) != confidenceRank(right.chunk.Confidence) {
			return confidenceRank(left.chunk.Confidence) > confidenceRank(right.chunk.Confidence)
		}
		return left.chunk.ChunkID < right.chunk.ChunkID
	})
	return reranked
}

type scoredChunk struct {
	chunk KBChunk
	score float64
}

func (r *Retriever) scoreChunk(queryTokens []string, productArea string, chunkIndex int, chunk KBChunk) float64 {
	if len(queryTokens) == 0 {
		return 0
	}
	score := patternsFieldWeight*r.bm25.patterns.score(chunkIndex, queryTokens) +
		titleFieldWeight*r.bm25.titles.score(chunkIndex, queryTokens) +
		contentFieldWeight*r.bm25.contents.score(chunkIndex, queryTokens)
	if score <= 0 {
		return 0
	}
	if productArea != "" && strings.EqualFold(productArea, chunk.ProductArea) {
		score += 2
	}
	return score
}

func chunkActiveAt(chunk KBChunk, now time.Time) bool {
	today := dateOnlyBeijing(now)
	if chunk.ValidFrom != "" {
		validFrom, err := time.ParseInLocation("2006-01-02", chunk.ValidFrom, beijingLocation)
		if err != nil || today.Before(validFrom) {
			return false
		}
	}
	if chunk.ValidTo != nil && strings.TrimSpace(*chunk.ValidTo) != "" {
		validTo, err := time.ParseInLocation("2006-01-02", *chunk.ValidTo, beijingLocation)
		if err != nil || today.After(validTo) {
			return false
		}
	}
	return true
}

func confidenceRank(confidence string) int {
	switch confidence {
	case confidenceHigh:
		return 2
	case confidenceMedium:
		return 1
	default:
		return 0
	}
}

func dateOnlyBeijing(t time.Time) time.Time {
	year, month, day := t.In(beijingLocation).Date()
	return time.Date(year, month, day, 0, 0, 0, 0, beijingLocation)
}

// cosineSimilarity must stay byte-equivalent to internal/embedding.Cosine and
// to scripts/rag_w0/retrieval_scoring.cosine_similarity. We re-implement it
// here (instead of importing internal/embedding) so the knowledge package
// stays free of the embedding-package dependency and the embedding test
// suite can verify the canonical implementation in isolation.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0.0
	}
	var dot, na, nb float64
	for i, x := range a {
		fx := float64(x)
		fy := float64(b[i])
		dot += fx * fy
		na += fx * fx
		nb += fy * fy
	}
	if na == 0.0 || nb == 0.0 {
		return 0.0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
