package engine

import (
	"context"
	"strings"

	"github.com/compshare-agent/internal/knowledge"
)

// disciplinedKQASynthesisOn gates the disciplined-synthesis recovery for an
// agent-loop knowledge_qa turn (the synthesis-discipline lever). Default false =>
// byte-identical: an uncited agent-loop synthesis keeps the existing
// retrySearchKnowledgeCitation fallback (re-prompt the heavy ReAct context once).
//
// MOTIVATION (kqa_agent_loop_ab_report.md, B4): the migration's residual refusals
// are NOT retrieval misses or cite-FORMAT misses — the right chunk is retrieved at
// rank 1, but flash's FREE ReAct synthesis intermittently omits the citation or
// dumps raw evidence, and the cite/leak guards then refuse a good answer. The fix
// is to make the FINAL ANSWER come from terminal RAG's proven disciplined
// cited-synthesis prompt (answerWithRetrievedEvidence: tight single-purpose prompt
// + its own cite-harder retry), run on the evidence the agent gathered via
// SearchKnowledge — importing terminal's reliability instead of hoping the loop
// reproduces it. Effective ONLY on an agent-loop knowledge_qa turn
// (knowledgeQAAgentLoopThisTurn); inert otherwise, so flag-off is byte-identical.
// Set once at boot from COMPSHARE_KQA_DISCIPLINED_SYNTHESIS (cmd); the Go-package
// default stays false so engine unit tests are unaffected.
var disciplinedKQASynthesisOn bool

// SetDisciplinedKQASynthesisEnabled toggles the disciplined-synthesis recovery.
// Boot-only (reversible by restart), mirroring SetKnowledgeQAAgentLoopEnabled.
func SetDisciplinedKQASynthesisEnabled(v bool) { disciplinedKQASynthesisOn = v }

// DisciplinedKQASynthesisEnabled reports whether the disciplined-synthesis recovery is on.
func DisciplinedKQASynthesisEnabled() bool { return disciplinedKQASynthesisOn }

// synthesizeKnowledgeQAFromLedger writes the final answer for an agent-loop
// knowledge_qa turn using terminal RAG's disciplined cited-synthesis prompt
// (answerWithRetrievedEvidence) on the evidence the agent gathered via
// SearchKnowledge this turn. This is the synthesis-discipline lever: instead of the
// free ReAct synthesis (heavy context, where flash drops the cite / dumps raw text),
// the answer is written by the tight, single-purpose prompt the terminal route uses
// — which already over-refuses ~never and emits positional [n] reliably.
//
// Returns (stripped display answer, true) only when terminal's synthesis produced a
// clean, cited, non-leaking answer; ("", false) on no-evidence / refusal / token
// budget / leak — in which case the caller keeps the existing fallback (cite-retry /
// refusal), so this never does WORSE than the free-write path.
//
// The [n] markers terminal emits number the references in `evidences` order, which
// equals the per-turn ledger order for the common single-SearchKnowledge-call turn
// (the forced first hop). A turn with multiple SearchKnowledge calls could drift the
// numbering vs the deduped ledger; that edge case keeps the conservative refusal
// (fail-safe), never a mis-citation.
func (e *Engine) synthesizeKnowledgeQAFromLedger(ctx context.Context, userMsg string) (string, bool) {
	if len(e.searchKnowledgeHitsThisTurn) == 0 {
		return "", false
	}
	// The gathered hits already passed the SearchKnowledge relevance floor (weak hits
	// were dropped to nil in executeSearchKnowledge), so they are NOT weak evidence —
	// pass weak=false so terminal uses its standard (not weak-mode) RAG prompt.
	evidences, err := evidencesFromRetrievalHits(e.searchKnowledgeHitsThisTurn, knowledge.NormalizeQuery(userMsg))
	if err != nil || len(evidences) == 0 {
		return "", false
	}
	reply, _, refusedReason, _, aerr := e.answerWithRetrievedEvidence(ctx, userMsg, evidences, false, nil)
	if aerr != nil || refusedReason != "" {
		return "", false
	}
	// answerWithRetrievedEvidence guarantees a positional-cited answer when
	// refusedReason=="" (else it returns the canned refusal with a reason). Re-run the
	// no-raw-leak guard the agent-loop enforces (terminal relies on its paraphrase
	// prompt and does not leak-check); a leak keeps the conservative fallback.
	if strings.TrimSpace(reply) == "" || isKnowledgeRefusal(reply) {
		return "", false
	}
	if knowledge.ValidateNoRawEvidenceLeak(reply, e.searchKnowledgeHitsThisTurn) != nil {
		return "", false
	}
	return stripCitationMarkers(reply), true
}
