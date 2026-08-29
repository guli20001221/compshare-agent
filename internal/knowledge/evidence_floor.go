package knowledge

// Relevance floors are shared by every model-facing retrieval lane. A low-score hit must not be
// rejected by the outer Agent yet become readable by the higher-authority in-instance Agent.
const (
	WeakEvidenceBM25Threshold     = 55.0
	WeakEvidenceSemanticThreshold = 0.5
)

// AppliedEvidenceFloor reports the calibrated floor for this result and whether its score scale can
// be judged. Unknown scales and qwen3 RRF fallback scores deliberately remain unjudged: applying a
// BM25 or semantic threshold to those values would reject an entire unrelated scale.
func AppliedEvidenceFloor(items []RetrievalHit, hybridMode string, rerankerScored bool) (float64, bool) {
	if len(items) == 0 || ScoreScaleFor(hybridMode) == ScoreScaleUnknown {
		return 0, false
	}
	if hybridMode == RetrievalModeQwen3RRF && !rerankerScored {
		return 0, false
	}
	return WeakEvidenceThresholdFor(hybridMode), true
}

// IsWeakEvidence applies the same top-hit relevance decision to outer chat and inner SSH retrieval.
func IsWeakEvidence(items []RetrievalHit, hybridMode string, rerankerScored bool) bool {
	floor, judged := AppliedEvidenceFloor(items, hybridMode, rerankerScored)
	return judged && items[0].Score < floor
}

// WeakEvidenceThresholdFor maps a known retrieval score scale to its calibrated floor. Callers
// should use AppliedEvidenceFloor rather than applying this value to an unknown scale.
func WeakEvidenceThresholdFor(hybridMode string) float64 {
	if ScoreScaleFor(hybridMode) == ScoreScaleSemantic {
		return WeakEvidenceSemanticThreshold
	}
	return WeakEvidenceBM25Threshold
}
