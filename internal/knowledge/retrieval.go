package knowledge

// Retrieval modes are what the remote knowledge service reports for a search.
// Each names the pipeline that produced the scores, which is what decides the
// scale those scores live on (ScoreScaleFor).
const (
	RetrievalModeBM25Only     = "bm25_only"
	RetrievalModeHybridCosine = "hybrid_cosine"
	RetrievalModeHybridRerank = "hybrid_rerank"
	RetrievalModeQwen3Full    = "qwen3_full"
	RetrievalModeQwen3RRF     = "qwen3_rrf"
	// RetrievalModeBM25Fallback is a hybrid path that lost its embedding step
	// mid-flight, so the scores are BM25 scores again. KnownRetrievalMode and the
	// engine's floor tables both have to agree that it is judgeable.
	RetrievalModeBM25Fallback = "bm25_fallback"
	// RetrievalModeUnknownRemote marks a result whose SCORE SCALE this process
	// cannot identify: the remote retriever reported a mode that is not in the
	// list above (or reported none at all). It is deliberately NOT one of the
	// modes — it is the absence of one, named so downstream code can branch on
	// it instead of guessing.
	//
	// The empty string keeps meaning "hand-written fixture" for the tests that
	// rely on it; see ScoreScaleFor.
	RetrievalModeUnknownRemote = "unknown_remote"
)

// ScoreScale names the numeric scale a retrieval mode's Score values live on.
// It exists because a relevance threshold is only meaningful against a scale,
// never against a mode: two modes that score the same way must be judged the
// same way, and a mode nobody has classified must not be judged at all.
type ScoreScale int

const (
	// ScoreScaleUnknown means no consumer may compare these scores to a
	// threshold. It is the zero value ON PURPOSE: a mode that nobody classified
	// falls here, so forgetting to classify one degrades to "decline to judge"
	// rather than to some arbitrary calibrated scale.
	ScoreScaleUnknown ScoreScale = iota
	// ScoreScaleBM25 is the unbounded 0..N Okapi BM25 sum.
	ScoreScaleBM25
	// ScoreScaleSemantic is the bounded [0,1] output of a cross-encoder reranker
	// or a cosine similarity.
	ScoreScaleSemantic
)

// ScoreScaleFor is THE mapping from retrieval mode to score scale, and the only
// place a new mode has to be classified.
//
// It is deliberately in this package and not in the consumer: which scale a
// pipeline emits is a property of the pipeline, while the threshold calibrated
// against that scale is a property of the answer policy. Splitting them that way
// is what makes drift impossible — a consumer keyed by scale has three cases and
// can be exhaustive, whereas a consumer keyed by mode has to be edited every
// time a mode is added, and silently falls through when it is not.
func ScoreScaleFor(mode string) ScoreScale {
	switch mode {
	case RetrievalModeBM25Only, RetrievalModeBM25Fallback:
		return ScoreScaleBM25
	case RetrievalModeHybridCosine, RetrievalModeHybridRerank,
		RetrievalModeQwen3Full, RetrievalModeQwen3RRF:
		return ScoreScaleSemantic
	case "":
		// NOT a mode: the empty string is what a hand-written RetrievalResult{}
		// fixture leaves behind, and a body of tests depends on those being
		// judged on the BM25 scale. Kept explicit rather than folded into the
		// default so that "unclassified" keeps meaning unclassified.
		return ScoreScaleBM25
	default:
		return ScoreScaleUnknown
	}
}

// KnownRetrievalMode reports whether mode is something a remote retriever may
// claim and have believed. It is derived from ScoreScaleFor so the adapter's
// gate and the engine's floors cannot disagree: classify a new mode once, and
// both start honoring it in the same change.
//
// The empty string is excluded even though ScoreScaleFor gives it a scale. The
// two answer different questions — "what does this process assume about these
// numbers" versus "may a remote service assert this" — and a remote that names
// no mode has asserted nothing.
func KnownRetrievalMode(mode string) bool {
	return mode != "" && ScoreScaleFor(mode) != ScoreScaleUnknown
}

// AllRetrievalModes lists every mode this build classifies, so a test can
// enumerate them rather than restating a list that would then have to be kept
// in sync.
func AllRetrievalModes() []string {
	return []string{
		RetrievalModeBM25Only,
		RetrievalModeHybridCosine,
		RetrievalModeHybridRerank,
		RetrievalModeQwen3Full,
		RetrievalModeQwen3RRF,
		RetrievalModeBM25Fallback,
	}
}

// RetrievalResult is one search against the remote knowledge service.
type RetrievalResult struct {
	Enabled         bool
	KBVersion       string
	QueryNormalized string
	Hits            []KBChunk
	HitItems        []RetrievalHit
	Empty           bool
	// SearchID is the opaque, short-lived capability returned by the remote
	// compshare-kb MCP search. It is intentionally carried only with one
	// retrieval result; the Engine keeps it in current-turn state and supplies it
	// to ReadChunks, never to a process-wide retriever cache.
	SearchID string
	// Unavailable distinguishes an operational retrieval failure (for example a
	// remote MCP timeout or no active release) from a successful Empty search.
	Unavailable   bool
	FailureReason string
	// HybridMode is the retrieval mode the service reported for Hits, one of the
	// RetrievalMode* constants; RetrievalModeUnknownRemote when it reported a mode
	// this build cannot classify. Empty only when retrieval is disabled (no
	// retriever configured).
	HybridMode string
	// HybridFallbackReason is non-empty only when HybridMode == "bm25_fallback".
	// One of "embedding_timeout" | "embedding_error" | "embedding_empty".
	HybridFallbackReason string
	// EmbeddingLatencyMS is the service's wall-clock time in its embedder, in
	// milliseconds. Pointer is intentional to distinguish three states:
	//   - nil:    embedder was not invoked (bm25_only, or an empty pool).
	//   - *0:     embedder returned in < 1ms.
	//   - *>0:    actual round-trip.
	EmbeddingLatencyMS *int64
	// EmbeddingModel labels which embedder produced the cosine scores. Empty
	// when no embedder was invoked.
	EmbeddingModel string
	// RerankerMode labels which reranker model produced the final ranking.
	// Empty when the reranker stage was not engaged.
	RerankerMode string
	// RerankerLatencyMS mirrors EmbeddingLatencyMS three-state semantics for the
	// reranker stage.
	RerankerLatencyMS *int64
	// RerankerFallbackReason is non-empty only when the reranker stage was
	// attempted but failed and the service returned the prior stage's top-K
	// instead. One of "reranker_timeout" | "reranker_error" | "reranker_empty".
	RerankerFallbackReason string
}

type RetrievalHit struct {
	Chunk KBChunk
	Score float64
	Kept  bool
}
