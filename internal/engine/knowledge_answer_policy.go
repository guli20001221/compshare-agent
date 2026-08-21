package engine

// Knowledge-answer validation is typography-only and fail-open. It records weak
// evidence and verbatim echoes but never replaces the Agent's answer with a
// canned refusal.
const (
	weakEvidenceBM25Threshold     = 55.0
	weakEvidenceSemanticThreshold = 0.5

	rankingAmbiguousBM25Spread     = 5.0
	rankingAmbiguousSemanticSpread = 0.05
)
