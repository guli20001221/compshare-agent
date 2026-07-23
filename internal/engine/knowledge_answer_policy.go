package engine

const (
	// ragUngroundableReply is the ONLY fixed knowledge reply left. It is a security
	// stop for a persistent raw-evidence leak (a verbatim dump the retry could not
	// rewrite) — NOT a grounding refusal. Typography-only + fail-open never replaces
	// an ungrounded answer with a canned "知识库未覆盖" template (that string was a
	// lie 81% of the time in production: the evidence was there, the answer was
	// written, and a bracket-regex deleted it).
	ragUngroundableReply = "我查到了相关资料，但没能据此整理出完全有依据的回答。请把问题描述得更具体一些，我再试一次。"

	weakEvidenceBM25Threshold     = 55.0
	weakEvidenceSemanticThreshold = 0.5

	rankingAmbiguousBM25Spread     = 5.0
	rankingAmbiguousSemanticSpread = 0.05
)
