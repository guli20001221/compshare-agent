package engine

// The knowledge path has ZERO canned replies. Typography-only + fail-open never
// replaces an ungrounded answer with a "知识库未覆盖" template (that string was a lie
// 81% of the time in production: the evidence was there, the answer was written,
// and a bracket-regex deleted it), and the last fixed reply — the verbatim-dump
// stop — was retired once it was clear the corpus is entirely customer-safe, so
// the dump was a prose problem it "fixed" by deleting a correct answer.
const (
	weakEvidenceBM25Threshold     = 55.0
	weakEvidenceSemanticThreshold = 0.5

	rankingAmbiguousBM25Spread     = 5.0
	rankingAmbiguousSemanticSpread = 0.05
)
