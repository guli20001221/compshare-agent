package observability

// RefusalType* classify a RAG turn's no-grounded-answer outcome into the #5
// three-state taxonomy. The distinction drives
// different remediation: corpus_gap → expand the corpus; all_below_floor → tune
// the relevance floor; synthesis_refused → the LLM declined good evidence;
const (
	RefusalTypeCorpusGap        = "corpus_gap"
	RefusalTypeAllBelowFloor    = "all_below_floor"
	RefusalTypeSynthesisRefused = "synthesis_refused"
)

// DeriveRefusalType maps the recorded RefusedReason (+ FloorDroppedAll) onto the
// three-state taxonomy. Pure; called at Finish on the merged retrieval trace.
//
// The RefusedReason literals mirror the engine's RAG refusal producers
// (the knowledge Agent and emitSearchKnowledgeRetrievalTrace):
// "no_evidence", "weak_evidence", "refusal", "retry_no_cite",
// plus the infra failures "token_budget" / "llm_error". The engine-side literals
// are pinned by its own RefusedReason tests; this mapping is pinned by
// TestDeriveRefusalType — a drift on either side breaks one of the two.
//
// Empty when the turn did not emit a knowledge-coverage refusal: a clean answer,
// an infra failure (token_budget / llm_error — already attributed by
// outcome.terminated_by), or a floor-dropped turn that answered with general
// guidance instead of refusing (FloorDroppedAll is still recorded for that turn,
// so the floor activity stays queryable even though refusal_type is empty).
func (t RetrievalTrace) DeriveRefusalType() string {
	// A partial MCP outage makes corpus coverage indeterminate. Even if another
	// query in the same turn completed empty, do not send the resulting merged
	// trace down the corpus-gap remediation path; Unavailable carries the
	// operational signal explicitly.
	if t.Unavailable {
		return ""
	}
	switch t.RefusedReason {
	case "no_evidence":
		if t.FloorDroppedAll {
			return RefusalTypeAllBelowFloor
		}
		return RefusalTypeCorpusGap
	case "weak_evidence", "refusal", "retry_no_cite":
		return RefusalTypeSynthesisRefused
	default:
		// "", "token_budget", "llm_error" — not a knowledge-coverage refusal.
		return ""
	}
}
