package knowledge

import (
	"context"
	"errors"
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
	// rerankerPoolSize bounds the cosine-stage output handed to the reranker.
	// Larger pool = more recall headroom but more reranker latency / tokens.
	// 10 chosen so top-K=3 has cross-encoder reorder headroom without burning
	// extra tokens. B.0 probe confirmed 50-doc single batch works, so this is
	// well under server capacity.
	rerankerPoolSize = 10
	// RRF (qwen3_rrf mode) constants. Pool sizes widened to 50 for both
	// retrieval signals; rank-level fusion benefits from a deeper window than
	// the cascade path's 20-pool (Elastic + OpenSearch + Azure recommend
	// 50-100). rrfK=60 is the canonical smoothing constant from Cormack et al.
	// 2009 SIGIR and the default across Azure Cognitive Search, Elastic 8.8+
	// (rank_constant), OpenSearch 2.19+ score-ranker, Vespa, and LlamaIndex
	// QueryFusionRetriever. Do NOT tune without strong empirical justification.
	rrfBM25PoolSize  = 50
	rrfDensePoolSize = 50
	rrfK             = 60
)

// Retrieval modes select which retrieval pipeline the Retriever runs. The
// modes are env-driven via RAG_RETRIEVAL_MODE (parsed in cmd/trace.go);
// unset env falls back to legacy RAG_HYBRID_ENABLED behavior.
//
//	RetrievalModeBM25Only     — BM25 top-K, no embedding/reranker stage.
//	RetrievalModeHybridCosine — BM25 top-20 → cosine top-K. (legacy hybrid_on)
//	RetrievalModeHybridRerank — BM25 top-20 → cosine top-10 → reranker top-K.
//	                            Uses the text-embedding-3-large sidecar +
//	                            qwen3-reranker-8b cross-encoder.
//	RetrievalModeQwen3Full    — BM25 top-20 → cosine top-10 → reranker top-K.
//	                            Uses the qwen3-embedding-8b sidecar + the
//	                            qwen3-reranker-8b cross-encoder.
//	RetrievalModeQwen3RRF     — BM25 top-50 ⊕ dense-full-corpus top-50, fused
//	                            via Reciprocal Rank Fusion (k=60), reranker
//	                            top-K. Uses the same qwen3-embedding-8b
//	                            sidecar + qwen3-reranker-8b cross-encoder as
//	                            Qwen3Full but recovers BM25-zero-hit queries
//	                            via the dense leg (the cascade path skips
//	                            embedding entirely when BM25 returns nothing).
const (
	RetrievalModeBM25Only     = "bm25_only"
	RetrievalModeHybridCosine = "hybrid_cosine"
	RetrievalModeHybridRerank = "hybrid_rerank"
	RetrievalModeQwen3Full    = "qwen3_full"
	RetrievalModeQwen3RRF     = "qwen3_rrf"
)

// VectorEmbedder is satisfied by *internal/embedding.Client and by test
// doubles. Keeping it as a local interface lets the retriever stay free of
// the embedding package import dependency.
type VectorEmbedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// RerankerClient is satisfied by internal/reranker.Client. Same
// dependency-isolation pattern as VectorEmbedder: a local interface
// keeps the knowledge package free of the reranker package import.
type RerankerClient interface {
	Rerank(ctx context.Context, query string, docs []string, topN int) ([]RerankerResult, error)
}

// RerankerResult mirrors internal/reranker.Result so the knowledge package
// stays free of the reranker package import. Reranker implementations
// adapt their native result type via a thin wrapper (see
// cmd/trace.go rerankerClientFromEnv).
type RerankerResult struct {
	Index int
	Score float64
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
	// EmbeddingModel labels the embedder for trace observability (passed
	// through to RetrievalResult.EmbeddingModel). Examples:
	// "text-embedding-3-large", "qwen3-embedding-8b". Empty when no
	// embedder is configured.
	EmbeddingModel string
	// HybridContextTimeout bounds each query embedding call. Defaults to
	// 5s (matches internal/embedding p99 measurement) when zero or
	// negative. The retriever swallows embedding errors and falls back
	// to BM25 top-3 from its top-20 pool. Override in production via the
	// RAG_HYBRID_TIMEOUT_MS env var (parsed in cmd/trace.go).
	HybridContextTimeout time.Duration
	// Reranker, when non-nil, engages the cross-encoder rerank stage after
	// the cosine stage. Mode must be HybridRerank or Qwen3Full for the
	// stage to actually run. RerankerModel labels it for trace.
	Reranker      RerankerClient
	RerankerModel string
	// RerankerContextTimeout bounds each reranker call. Defaults to 5s
	// (B.0 probe measured ~3.8s for 50-doc single batch). Override in
	// production via RAG_RERANKER_TIMEOUT_MS.
	RerankerContextTimeout time.Duration
	// Mode selects the retrieval pipeline. Empty defaults to bm25_only when
	// no embedder is supplied, hybrid_cosine when an embedder+sidecar are.
	// Values other than the RetrievalMode* constants treated as defaults.
	Mode string
}

type RetrievalResult struct {
	Enabled         bool
	KBVersion       string
	QueryNormalized string
	Hits            []KBChunk
	HitItems        []RetrievalHit
	Empty           bool
	// HybridMode reports which retrieval path produced Hits. One of:
	//   "bm25_only"      — sidecar or embedder absent; BM25 top-K directly.
	//   "hybrid_cosine"  — BM25 top-20 → query-embedding cosine rerank → top-K.
	//   "bm25_fallback"  — hybrid path was taken but the embedding step failed
	//                      and the retriever fell back to BM25 top-K from the
	//                      same pool. HybridFallbackReason then carries why.
	// Empty string is reserved for the retrieval-disabled case (no retriever
	// configured); see internal/engine/engine.go's early-return branch.
	HybridMode string
	// HybridFallbackReason is non-empty only when HybridMode == "bm25_fallback".
	// One of "embedding_timeout" | "embedding_error" | "embedding_empty".
	HybridFallbackReason string
	// EmbeddingLatencyMS is the wall-clock time spent in r.embedder.Embed,
	// in milliseconds. Pointer is intentional to distinguish three states:
	//   - nil:    embedder was not invoked (HybridMode == "bm25_only", or
	//             hybrid configured but BM25 pool was empty so the embed
	//             call was skipped). Distinguishing "skipped" from a real
	//             "0ms" round-trip is why this is *int64, not int64+omitempty.
	//   - *0:     embedder returned in < 1ms (currently unreachable in
	//             production but reserved for future client-side cache hits).
	//   - *>0:    actual round-trip. On embedding_timeout this approximates
	//             the configured HybridContextTimeout (ctx-cancel latency).
	// Ops uses this to compute production p95/p99 latency distribution and
	// pick a principled HybridContextTimeout instead of blind tuning.
	EmbeddingLatencyMS *int64
	// EmbeddingModel labels which embedder produced the cosine scores.
	// Examples: "text-embedding-3-large", "qwen3-embedding-8b". Empty
	// when no embedder was invoked (bm25_only path).
	EmbeddingModel string
	// RerankerMode labels which reranker model produced the final ranking.
	// Empty when the reranker stage was not engaged. Non-empty examples:
	// "qwen3-reranker-8b". Distinguishes "reranker not configured for this
	// mode" (empty) from "reranker invoked" (model name).
	RerankerMode string
	// RerankerLatencyMS mirrors EmbeddingLatencyMS three-state semantics
	// for the reranker stage:
	//   - nil:    reranker was not invoked (mode != hybrid_rerank/qwen3_full,
	//             or pool empty so reranker call was skipped).
	//   - *0:     reranker returned in < 1ms (reserved).
	//   - *>0:    actual round-trip. On reranker_timeout approximates the
	//             configured RerankerContextTimeout.
	RerankerLatencyMS *int64
	// RerankerFallbackReason is non-empty only when the reranker stage was
	// attempted but failed and the retriever returned the prior stage's
	// top-K instead. One of "reranker_timeout" | "reranker_error" |
	// "reranker_empty". Empty when reranker succeeded or was not engaged.
	RerankerFallbackReason string
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
	embeddingModel   string
	hybridTimeout    time.Duration
	reranker         RerankerClient
	rerankerModel    string
	rerankerTimeout  time.Duration
	mode             string
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
		// Matches internal/embedding default; see embedding/client.go for
		// the p99 measurement rationale.
		hybridTimeout = 5 * time.Second
	}
	rerankerTimeout := opts.RerankerContextTimeout
	if rerankerTimeout <= 0 {
		// B.0 probe measured ~3.8s for 50-doc single batch; 5s default
		// matches the embedding timeout for symmetric latency budgeting.
		rerankerTimeout = 5 * time.Second
	}
	// Mode defaulting: empty mode falls back based on what's wired. This
	// preserves all pre-B.3 callers — they don't set Mode and get the same
	// behavior as before. Explicit Mode value wins.
	mode := opts.Mode
	if mode == "" {
		switch {
		case opts.Reranker != nil && opts.EmbeddingSidecar != nil && opts.Embedder != nil:
			mode = RetrievalModeHybridRerank
		case opts.EmbeddingSidecar != nil && opts.Embedder != nil:
			mode = RetrievalModeHybridCosine
		default:
			mode = RetrievalModeBM25Only
		}
	}
	return &Retriever{
		corpus:           corpus,
		topK:             topK,
		threshold:        threshold,
		now:              now,
		bm25:             newRetrievalBM25Index(corpus.Chunks),
		embeddingSidecar: opts.EmbeddingSidecar,
		embedder:         opts.Embedder,
		embeddingModel:   opts.EmbeddingModel,
		hybridTimeout:    hybridTimeout,
		reranker:         opts.Reranker,
		rerankerModel:    opts.RerankerModel,
		rerankerTimeout:  rerankerTimeout,
		mode:             mode,
	}
}

// hybridEnabled reports whether the retriever should take the BM25-top-20
// then embedding-rerank path. True when the configured mode includes a
// cosine stage AND the embedder+sidecar are wired.
func (r *Retriever) hybridEnabled() bool {
	if r.embeddingSidecar == nil || r.embedder == nil {
		return false
	}
	switch r.mode {
	case RetrievalModeHybridCosine, RetrievalModeHybridRerank,
		RetrievalModeQwen3Full, RetrievalModeQwen3RRF:
		return true
	}
	return false
}

// rerankerEnabled reports whether the configured mode includes the
// cross-encoder rerank stage AND a reranker client is wired.
func (r *Retriever) rerankerEnabled() bool {
	if r.reranker == nil {
		return false
	}
	switch r.mode {
	case RetrievalModeHybridRerank, RetrievalModeQwen3Full, RetrievalModeQwen3RRF:
		return true
	}
	return false
}

func (r *Retriever) Retrieve(question, productArea string) RetrievalResult {
	queryNormalized := NormalizeQuery(question)
	productArea = strings.TrimSpace(strings.ToLower(productArea))

	bm25Candidates := r.collectBM25Candidates(question, productArea)
	// Pool sizing is mode-aware: the cascade path (hybrid_cosine /
	// hybrid_rerank / qwen3_full) uses hybridBM25PoolSize=20 because it
	// only reranks within the BM25 top window. qwen3_rrf widens this to
	// rrfBM25PoolSize=50 to give the rank-level fusion a deeper signal
	// (Elastic + OpenSearch + Azure recommend 50-100 for RRF inputs).
	limit := r.topK
	if r.hybridEnabled() {
		if r.mode == RetrievalModeQwen3RRF {
			limit = rrfBM25PoolSize
		} else {
			limit = hybridBM25PoolSize
		}
	}
	if len(bm25Candidates) > limit {
		bm25Candidates = bm25Candidates[:limit]
	}

	finalCandidates := bm25Candidates
	// hybridMode tracks which retrieval path produced the final candidates.
	// Default to bm25_only; switched to hybrid_cosine / hybrid_rerank /
	// qwen3_full / qwen3_rrf when the corresponding mode ran successfully.
	// Switched to bm25_fallback when the embedder failed (cascade) or the
	// dense leg failed (qwen3_rrf) and the retriever returned BM25 pool
	// unchanged. embeddingLatencyMs is nil unless the embedder was actually
	// invoked, preserving the three-state distinction documented on
	// RetrievalResult.EmbeddingLatencyMS. The reranker stage layers on top
	// of the prior stage: when it runs, hybridMode stays at the prior label
	// + rerankerMode/rerankerLatencyMs are populated; on reranker failure,
	// hybridMode stays at the pre-reranker label and rerankerFallbackReason
	// carries the cause.
	hybridMode := "bm25_only"
	hybridFallbackReason := ""
	var embeddingLatencyMs *int64
	embeddingModel := ""
	rerankerMode := ""
	var rerankerLatencyMs *int64
	rerankerFallbackReason := ""
	rrfInfo := map[string]rrfRankInfo(nil)
	if r.mode == RetrievalModeQwen3RRF {
		// CRITICAL: do NOT gate on len(bm25Candidates) > 0. The entire
		// value of RRF is recovering BM25-zero-hit queries via the dense
		// leg. The cascade path skips the embedder when BM25 is empty
		// (because cosine over a 0-doc pool is degenerate) — RRF must
		// NOT inherit that behavior.
		denseTopN, denseFallbackReason, denseLatencyMs := r.denseFullSearch(question, rrfDensePoolSize)
		embeddingLatencyMs = &denseLatencyMs
		if denseFallbackReason != "" {
			// Dense leg failed — degrade to bm25-only (same semantics as
			// cascade's embedding_error fallback). BM25 may itself be
			// empty (legitimate "nothing matches"); that case still
			// returns no hits.
			finalCandidates = bm25Candidates
			hybridMode = "bm25_fallback"
			hybridFallbackReason = denseFallbackReason
		} else {
			embeddingModel = r.embeddingModel
			// Fusion window: when reranker is on, cap fused output at
			// rerankerPoolSize=10 (matches cascade's pre-reranker pool);
			// otherwise cap at topK so we don't carry candidates past
			// the topK truncation below.
			fuseTopN := r.topK
			if r.rerankerEnabled() {
				fuseTopN = rerankerPoolSize
			}
			fused, info := rrfFusion(bm25Candidates, denseTopN, fuseTopN)
			finalCandidates = fused
			rrfInfo = info
			hybridMode = "qwen3_rrf"

			if r.rerankerEnabled() && len(finalCandidates) > 0 {
				rerankedByRerank, rerankFallbackReason, rerankLatencyMs := r.rerankByReranker(question, finalCandidates)
				rerankerLatencyMs = &rerankLatencyMs
				if rerankFallbackReason != "" {
					rerankerFallbackReason = rerankFallbackReason
					// finalCandidates stays at RRF ranking; rrfInfo
					// already populated so trace fields are correct.
				} else {
					finalCandidates = rerankedByRerank
					rerankerMode = r.rerankerModel
					// hybridMode stays "qwen3_rrf" — reranker is a layer
					// on top, not a different retrieval mode. The reranker
					// overwrites scoredChunk.score (with relevance_score),
					// but rrfInfo[cid].FusionScore is preserved separately
					// so trace consumers can still recover the pre-rerank
					// fusion score.
				}
			}
		}
	} else if r.hybridEnabled() {
		if len(bm25Candidates) > 0 {
			reranked, fallbackReason, latencyMs := r.rerankByEmbedding(question, bm25Candidates)
			finalCandidates = reranked
			embeddingLatencyMs = &latencyMs
			if fallbackReason != "" {
				// Cosine stage failed: BM25 pool returned unchanged, mark
				// bm25_fallback, leave embeddingModel empty (no valid cosine
				// signal was produced), and skip reranker entirely.
				hybridMode = "bm25_fallback"
				hybridFallbackReason = fallbackReason
			} else {
				hybridMode = "hybrid_cosine"
				embeddingModel = r.embeddingModel
				// Cosine succeeded; if reranker is configured, layer it on
				// top of cosine top-N pool. Reranker failure does NOT
				// downgrade to bm25_fallback — we still have valid cosine
				// scores, so the trace records hybrid_cosine + a reranker
				// fallback reason instead.
				if r.rerankerEnabled() {
					rerankerPool := finalCandidates
					if len(rerankerPool) > rerankerPoolSize {
						rerankerPool = rerankerPool[:rerankerPoolSize]
					}
					rerankedByRerank, rerankFallbackReason, rerankLatencyMs := r.rerankByReranker(question, rerankerPool)
					rerankerLatencyMs = &rerankLatencyMs
					if rerankFallbackReason != "" {
						rerankerFallbackReason = rerankFallbackReason
						// Keep finalCandidates = cosine ranking (the prior
						// stage's order). hybridMode stays hybrid_cosine.
					} else {
						finalCandidates = rerankedByRerank
						rerankerMode = r.rerankerModel
						switch r.mode {
						case RetrievalModeQwen3Full:
							hybridMode = "qwen3_full"
						case RetrievalModeHybridRerank:
							hybridMode = "hybrid_rerank"
						}
					}
				}
			}
		} else {
			// BM25 pool empty: embedder was never invoked, but the configured
			// path is hybrid. Report hybrid_cosine with no fallback so ops
			// dashboards don't see this as a "BM25-only retriever".
			// EmbeddingLatencyMS stays nil since the embedder was not called.
			hybridMode = "hybrid_cosine"
		}
	}
	_ = rrfInfo // commit 5 plumbs rrfInfo[cid] into RetrievalHit trace fields

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
		Enabled:                true,
		KBVersion:              r.corpus.KBVersion,
		QueryNormalized:        queryNormalized,
		Hits:                   hits,
		HitItems:               hitItems,
		Empty:                  len(hits) == 0,
		HybridMode:             hybridMode,
		HybridFallbackReason:   hybridFallbackReason,
		EmbeddingLatencyMS:     embeddingLatencyMs,
		EmbeddingModel:         embeddingModel,
		RerankerMode:           rerankerMode,
		RerankerLatencyMS:      rerankerLatencyMs,
		RerankerFallbackReason: rerankerFallbackReason,
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
// Returns (reranked, fallbackReason, latencyMs):
//   - On success: reranked candidates, "", actual embed round-trip ms
//   - On embedding error: bm25Pool unchanged, reason, time-to-error ms
//     (reason "embedding_timeout" via errors.Is(err, ctx.DeadlineExceeded),
//     else "embedding_error"; per user 2026-05-17 spec
//     "embedding 失败时降级 BM25", no retry)
//   - On empty query vector: bm25Pool unchanged, "embedding_empty", ms
//
// latencyMs is always measured (success or failure) and the caller wraps
// it into RetrievalResult.EmbeddingLatencyMS (*int64) so ops can compute
// production p95/p99 from the trace JSONL and pick a principled timeout.
//
// The Python eval pipeline (scripts/rag_w0/evaluate_retrieval.py --mode
// hybrid) must produce the same final top-K chunk_id sets on the same
// inputs for the 377-Q parity contract to hold.
func (r *Retriever) rerankByEmbedding(question string, bm25Pool []scoredChunk) ([]scoredChunk, string, int64) {
	ctx, cancel := context.WithTimeout(context.Background(), r.hybridTimeout)
	defer cancel()
	start := time.Now()
	queryVec, err := r.embedder.Embed(ctx, question)
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		log.Printf("rag.hybrid: query embedding failed in %dms, falling back to BM25 top-%d: %v", latencyMs, r.topK, err)
		reason := "embedding_error"
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "embedding_timeout"
		}
		return bm25Pool, reason, latencyMs
	}
	if len(queryVec) == 0 {
		log.Printf("rag.hybrid: query embedding empty in %dms, falling back to BM25 top-%d", latencyMs, r.topK)
		return bm25Pool, "embedding_empty", latencyMs
	}
	reranked := make([]scoredChunk, 0, len(bm25Pool))
	for _, c := range bm25Pool {
		chunkVec, ok := r.embeddingSidecar.Vectors[c.chunk.ChunkID]
		if !ok {
			// Sidecar/corpus drift past LoadPinnedCorpusWithEmbeddings's bijection
			// guarantee should be impossible; log and skip this candidate so the
			// hybrid path stays safe even when the invariant is somehow violated.
			// This is NOT counted as a fallback (we still produce cosine-ranked
			// results from the surviving candidates).
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
	return reranked, "", latencyMs
}

// denseFullSearch ranks ALL active corpus chunks by cosine similarity to
// the query embedding and returns the top-N. Used by the qwen3_rrf path
// as one of the two ranked lists fed to rrfFusion.
//
// CRITICAL invariant: iterate r.corpus.Chunks (the active set produced by
// LoadPinnedCorpusWithEmbeddings, post any corpus-level filtering) NOT
// r.embeddingSidecar.Vectors (the raw chunk_id→vector map). The sidecar's
// keyset is a superset of the active corpus when chunks are excluded at
// load time (e.g. via valid_from / valid_to / confidence filters); using
// the sidecar map directly would resurrect dropped chunks. Tested in
// TestDenseFullSearchIteratesCorpusNotSidecar.
//
// Returns (topN, fallbackReason, latencyMs):
//   - On success: top-N cosine-ranked candidates, "", actual round-trip ms
//   - On embedder error: nil, "embedding_error" (or "embedding_timeout"
//     when err Is context.DeadlineExceeded), time-to-error ms
//   - On empty query vector: nil, "embedding_empty", ms
//
// Complexity O(|corpus| × dim) per query. At 228 chunks × 4096 dim this
// is ~1M float-mul-add → ~5-10ms on modern CPU; full-corpus linear scan
// is fine until corpus size exceeds ~1k chunks. Revisit ANN (HNSW) above
// that scale.
func (r *Retriever) denseFullSearch(question string, topN int) ([]scoredChunk, string, int64) {
	ctx, cancel := context.WithTimeout(context.Background(), r.hybridTimeout)
	defer cancel()
	start := time.Now()
	queryVec, err := r.embedder.Embed(ctx, question)
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		log.Printf("rag.rrf: dense embedding failed in %dms: %v", latencyMs, err)
		reason := "embedding_error"
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "embedding_timeout"
		}
		return nil, reason, latencyMs
	}
	if len(queryVec) == 0 {
		log.Printf("rag.rrf: dense embedding empty in %dms", latencyMs)
		return nil, "embedding_empty", latencyMs
	}

	now := r.now()
	candidates := make([]scoredChunk, 0, len(r.corpus.Chunks))
	for _, chunk := range r.corpus.Chunks {
		// Active-set guard parallels collectBM25Candidates: chunks that
		// failed the valid_from/valid_to gate or were dropped for low
		// confidence must NOT surface here, even if the sidecar happens
		// to still hold their vector.
		if !chunkActiveAt(chunk, now) || chunk.Confidence == confidenceLow {
			continue
		}
		vec, ok := r.embeddingSidecar.Vectors[chunk.ChunkID]
		if !ok {
			// LoadPinnedCorpusWithEmbeddings's bijection check should make
			// this impossible; defensive skip so dense ranking still
			// produces a result if the invariant is ever violated.
			log.Printf("rag.rrf: sidecar missing vector for chunk %q, skipping in dense scan", chunk.ChunkID)
			continue
		}
		candidates = append(candidates, scoredChunk{
			chunk: chunk,
			score: cosineSimilarity(queryVec, vec),
		})
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
	if topN > 0 && len(candidates) > topN {
		candidates = candidates[:topN]
	}
	return candidates, "", latencyMs
}

// rerankByReranker takes the cosine-stage output and reorders it using the
// configured cross-encoder reranker. The pool is already cosine-ranked, so
// reranker failure can fall back to the cosine ranking cleanly without
// re-running anything.
//
// Returns (reranked, fallbackReason, latencyMs):
//   - On success: reranker-scored candidates (scoredChunk.score becomes the
//     relevance_score), "", actual call round-trip ms
//   - On reranker error: cosinePool unchanged (caller stays on cosine
//     ranking), reason ∈ {reranker_timeout, reranker_error}, time-to-error
//   - On empty results from server: cosinePool unchanged, "reranker_empty"
//
// latencyMs is always measured. Caller wraps into
// RetrievalResult.RerankerLatencyMS so ops can tune RAG_RERANKER_TIMEOUT_MS
// from production p95/p99.
func (r *Retriever) rerankByReranker(question string, cosinePool []scoredChunk) ([]scoredChunk, string, int64) {
	if len(cosinePool) == 0 {
		// Caller guards against this but defend defensively.
		return cosinePool, "", 0
	}
	docs := make([]string, len(cosinePool))
	for i, c := range cosinePool {
		// Mirror scripts/rag_w0/build_corpus_embeddings.py chunk_repr:
		// title + question_patterns + truncated content. The reranker
		// scores (query, doc-text) pairs, so passing the same chunk
		// representation the embedder saw keeps the two ranking signals
		// comparable.
		docs[i] = chunkReprForRerank(c.chunk)
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.rerankerTimeout)
	defer cancel()
	start := time.Now()
	results, err := r.reranker.Rerank(ctx, question, docs, r.topK)
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		log.Printf("rag.reranker: rerank failed in %dms, falling back to cosine top-%d: %v", latencyMs, r.topK, err)
		reason := "reranker_error"
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "reranker_timeout"
		}
		return cosinePool, reason, latencyMs
	}
	if len(results) == 0 {
		log.Printf("rag.reranker: empty results in %dms, falling back to cosine top-%d", latencyMs, r.topK)
		return cosinePool, "reranker_empty", latencyMs
	}
	reranked := make([]scoredChunk, 0, len(results))
	for _, res := range results {
		if res.Index < 0 || res.Index >= len(cosinePool) {
			log.Printf("rag.reranker: out-of-range index %d (pool=%d), skipping", res.Index, len(cosinePool))
			continue
		}
		reranked = append(reranked, scoredChunk{
			chunk: cosinePool[res.Index].chunk,
			score: res.Score,
		})
	}
	if len(reranked) == 0 {
		// All indexes out of range — treat as empty.
		log.Printf("rag.reranker: all results invalid, falling back to cosine top-%d", r.topK)
		return cosinePool, "reranker_empty", latencyMs
	}
	// Server returns desc-sorted (and reranker.Client re-sorts defensively),
	// so cosinePool[res.Index] is already in the right order. No re-sort
	// here; preserve the reranker's relevance ordering verbatim.
	return reranked, "", latencyMs
}

// chunkReprForRerank produces the text the reranker sees per chunk. Must
// stay byte-equivalent to scripts/rag_w0/build_corpus_embeddings.py
// chunk_repr (no TrimSpace on title, no strip elsewhere) — both ranking
// signals (cosine + cross-encoder) score the same chunk representation
// so their scores remain comparable.
func chunkReprForRerank(c KBChunk) string {
	title := c.Title
	patterns := strings.Join(c.QuestionPatterns, " | ")
	content := c.Content
	const maxContentRunes = 1800 // mirror build_corpus_embeddings.py:MAX_CONTENT_RUNES_FOR_EMB
	runes := []rune(content)
	if len(runes) > maxContentRunes {
		content = string(runes[:maxContentRunes])
	}
	return "标题: " + title + "\n常见问法: " + patterns + "\n正文: " + content
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

// rrfRankInfo carries per-chunk diagnostics from a Reciprocal Rank Fusion
// pass. Used by the qwen3_rrf retrieval path to populate RetrievalHit's
// debug fields so trace consumers can attribute "why did this chunk rise
// to the top": was it BM25-driven, dense-driven, or fused from both?
//
// All ranks are 1-indexed. Zero means "absent from that input list".
// FusionScore is preserved separately because a downstream reranker may
// overwrite RetrievalHit.Score, and we still want to debug "high RRF
// score but reranker demoted it" cases from trace alone.
type rrfRankInfo struct {
	BM25Rank    int
	DenseRank   int
	FusionRank  int
	FusionScore float64
}

// rrfFusion combines two ranked lists via Reciprocal Rank Fusion with the
// canonical k=60 smoothing constant (rrfK). Chunks present in only one
// input list still appear in the output (the absent-list contribution is
// zero). Ties break on chunk_id ascending for cross-run determinism.
//
// Returns (fused, info):
//   - fused: scoredChunks ranked desc by RRF score, truncated to topN
//   - info:  map keyed by chunk_id with bm25/dense/fusion rank + the
//     pre-reranker fusion score (see rrfRankInfo doc for why it's
//     preserved alongside the score in scoredChunk.score)
//
// Industry references for k=60 + rank-based fusion:
//   - Cormack, Clarke, Buettcher 2009. "Reciprocal Rank Fusion outperforms
//     Condorcet and individual Rank Learning Methods." SIGIR.
//   - Microsoft Azure Cognitive Search hybrid ranking docs.
//   - Elastic 8.8+ rank_constant default + OpenSearch 2.19+ score-ranker.
//   - Vespa phased ranking + LlamaIndex QueryFusionRetriever.
func rrfFusion(bm25List, denseList []scoredChunk, topN int) ([]scoredChunk, map[string]rrfRankInfo) {
	scores := make(map[string]float64, len(bm25List)+len(denseList))
	chunks := make(map[string]KBChunk, len(bm25List)+len(denseList))
	ranks := make(map[string]rrfRankInfo, len(bm25List)+len(denseList))

	for i, c := range bm25List {
		cid := c.chunk.ChunkID
		scores[cid] += 1.0 / float64(rrfK+i+1)
		chunks[cid] = c.chunk
		info := ranks[cid]
		info.BM25Rank = i + 1
		ranks[cid] = info
	}
	for i, c := range denseList {
		cid := c.chunk.ChunkID
		scores[cid] += 1.0 / float64(rrfK+i+1)
		chunks[cid] = c.chunk
		info := ranks[cid]
		info.DenseRank = i + 1
		ranks[cid] = info
	}

	fused := make([]scoredChunk, 0, len(scores))
	for cid, score := range scores {
		fused = append(fused, scoredChunk{
			chunk: chunks[cid],
			score: score,
		})
		info := ranks[cid]
		info.FusionScore = score
		ranks[cid] = info
	}
	sort.SliceStable(fused, func(i, j int) bool {
		if fused[i].score != fused[j].score {
			return fused[i].score > fused[j].score
		}
		return fused[i].chunk.ChunkID < fused[j].chunk.ChunkID
	})
	for i := range fused {
		cid := fused[i].chunk.ChunkID
		info := ranks[cid]
		info.FusionRank = i + 1
		ranks[cid] = info
	}
	if topN > 0 && len(fused) > topN {
		fused = fused[:topN]
	}
	return fused, ranks
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
