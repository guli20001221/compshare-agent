package engine

// knowledgeAnswerVerifierOn gates the model-assisted semantic verifier used at
// the knowledge_qa agent-loop exit. It is intentionally separate from the
// legacy citation-format validator: a citation proves attribution, not truth.
//
// Disabled is fail-closed. Once SearchKnowledge has supplied evidence, the
// engine will not release an unchecked answer; it returns the honest
// ragUngroundableReply instead. Deployment enables this explicitly after wiring
// the deterministic quote, unknown-chunk, raw-leak, clause-coverage and obvious-
// contradiction checks around the model verdict. The Go package default stays
// false so tests and embedders must opt in deliberately.
var knowledgeAnswerVerifierOn bool

// SetKnowledgeAnswerVerifierEnabled toggles semantic knowledge-answer
// verification. Boot-only, mirroring SetKnowledgeQAAgentLoopEnabled.
func SetKnowledgeAnswerVerifierEnabled(v bool) { knowledgeAnswerVerifierOn = v }

// KnowledgeAnswerVerifierEnabled reports whether semantic verification is on.
func KnowledgeAnswerVerifierEnabled() bool { return knowledgeAnswerVerifierOn }
