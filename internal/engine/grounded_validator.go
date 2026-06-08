package engine

// groundedAnswerValidatorOn gates the route-independent grounded-answer (cite +
// leak) validator on the agentic SearchKnowledge synthesis (#126). Default false
// => byte-identical: the SearchKnowledge tool result carries no cite_protocol and
// guardSearchKnowledgeSynthesis runs ONLY the pre-existing no-raw-leak check.
//
// Deliberately SEPARATE from the COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE gate (which is
// default-ON in production): the cite contract must stay default-off until a
// flag-on eval proves the agent attributes its answer at the hard-gate bar
// (100% cite-or-refuse / 0 raw-leak). Flipping it on is a separate, eval-gated PR.
// Set once at boot from COMPSHARE_RAG_GROUNDED_VALIDATOR (cmd); the Go-package
// default stays false so engine/tools unit tests are unaffected.
var groundedAnswerValidatorOn bool

// SetGroundedAnswerValidatorEnabled toggles the grounded-answer validator.
// Boot-only (reversible by restart), mirroring tools.SetAgenticSearchKnowledgeEnabled.
func SetGroundedAnswerValidatorEnabled(v bool) { groundedAnswerValidatorOn = v }

// GroundedAnswerValidatorEnabled reports whether the validator is on.
func GroundedAnswerValidatorEnabled() bool { return groundedAnswerValidatorOn }

// searchKnowledgeCiteProtocol instructs the agent to attribute each conclusion to
// the evidence it used, in a machine-checkable [[chunk_id]] form, and to omit any
// claim it cannot ground. Injected into the SearchKnowledge tool result ONLY when
// the grounded-answer validator is on; flag-off the result JSON is byte-identical.
const searchKnowledgeCiteProtocol = "回答时，对每条结论在句末用 [[chunk_id]] 标注其所依据的条目(chunk_id 见上方 items)；无法在所给证据中找到依据的内容请勿写出，必要时直说资料未覆盖。"

// searchKnowledgeLedgerTurnMaxItems caps the per-turn ChunkID-keyed evidence ledger
// (the union of every SearchKnowledge call's items this turn). Set well above any
// realistic per-turn distinct-chunk count (the ReAct loop is bounded by
// maxReActRounds and each SearchKnowledge call surfaces <=DefaultEvidenceLedgerMaxItems
// items), so a ChunkID shown to the agent stays citable. If the cap were ever
// exceeded the failure is fail-SAFE: a citation to a dropped item lands in
// UnknownCitations → conservative refusal, never a fabricated citation.
const searchKnowledgeLedgerTurnMaxItems = 256
