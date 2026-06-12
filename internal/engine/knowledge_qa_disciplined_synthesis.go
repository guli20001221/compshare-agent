package engine

import (
	"context"
	"strings"

	"github.com/compshare-agent/internal/knowledge"
)

// disciplinedKnowledgeQASynthesisOn gates the disciplined-synthesis recovery for an
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
// Set once at boot from COMPSHARE_KnowledgeQA_DISCIPLINED_SYNTHESIS (cmd); the Go-package
// default stays false so engine unit tests are unaffected.
var disciplinedKnowledgeQASynthesisOn bool

// SetDisciplinedKnowledgeQASynthesisEnabled toggles the disciplined-synthesis recovery.
// Boot-only (reversible by restart), mirroring SetKnowledgeQAAgentLoopEnabled.
func SetDisciplinedKnowledgeQASynthesisEnabled(v bool) { disciplinedKnowledgeQASynthesisOn = v }

// DisciplinedKnowledgeQASynthesisEnabled reports whether the disciplined-synthesis recovery is on.
func DisciplinedKnowledgeQASynthesisEnabled() bool { return disciplinedKnowledgeQASynthesisOn }

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
	// DELIBERATELY no separate no-raw-leak guard here: this synthesis IS the terminal
	// route's answerWithRetrievedEvidence, which terminal RAG itself runs WITHOUT a leak
	// check. A how-to knowledge answer legitimately reproduces a command / code snippet
	// (e.g. the torchrun + DDP boilerplate) verbatim from the evidence — that is the
	// answer, not a leak. Re-applying the agent-loop's prose-oriented leak guard here
	// flagged those code answers and forced the conservative fallback, which is exactly
	// why the agent loop over-refused code-heavy probes (DDP) where terminal does not.
	// Matching terminal (no leak check) is the whole point of the convergence.
	if strings.TrimSpace(reply) == "" || isKnowledgeRefusal(reply) {
		return "", false
	}
	return stripCitationMarkers(reply), true
}

// synthesizeOnBudgetExceeded writes a final grounded answer from the evidence
// SearchKnowledge already gathered this turn, for the PR2 budget policy: when
// the per-turn token budget is exhausted but evidence is in hand, deliver an
// answer grounded on it rather than discarding the turn for a bare
// "请简化问题". Returns ("", false) when nothing was retrieved or the evidence
// can't produce a clean cited answer — the caller then keeps the budget
// refusal (the "no evidence → refuse, never fabricate" guard).
//
// Independent of disciplinedKnowledgeQASynthesisOn: that flag gates the NORMAL
// (under-budget) synthesis path, whereas this is a budget-recovery path that
// only fires once the cap is already blown. It reuses the same primitive,
// which is itself budget-aware — answerWithRetrievedEvidence delivers a
// grounded answer even over cap, suppressing only its extra cite-retry.
func (e *Engine) synthesizeOnBudgetExceeded(ctx context.Context, userMsg string) (string, bool) {
	if len(e.searchKnowledgeHitsThisTurn) == 0 {
		return "", false
	}
	return e.synthesizeKnowledgeQAFromLedger(ctx, userMsg)
}
