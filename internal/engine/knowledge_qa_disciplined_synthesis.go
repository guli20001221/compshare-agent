package engine

import "context"

// disciplinedKnowledgeQASynthesisOn gates the one bounded repair attempt after
// the unified knowledge-answer verifier rejects a draft. The repair returns an
// answer and its evidence proof in the same response; it no longer borrows the
// separate terminal-RAG synthesis path or retries merely to add punctuation.
// Default false in the package; deployment config decides whether repair is on.
var disciplinedKnowledgeQASynthesisOn bool

// SetDisciplinedKnowledgeQASynthesisEnabled toggles the disciplined-synthesis recovery.
// Boot-only (reversible by restart), mirroring SetKnowledgeQAAgentLoopEnabled.
func SetDisciplinedKnowledgeQASynthesisEnabled(v bool) { disciplinedKnowledgeQASynthesisOn = v }

// DisciplinedKnowledgeQASynthesisEnabled reports whether the disciplined-synthesis recovery is on.
func DisciplinedKnowledgeQASynthesisEnabled() bool { return disciplinedKnowledgeQASynthesisOn }

// synthesizeKnowledgeQAFromLedger is the live agent-loop repair seam. It uses the
// exact per-turn EvidenceLedger (including its resolved query) and accepts only a
// response whose answer and claim proof pass the same deterministic checks as the
// main verifier. Keeping this seam live preserves the existing deployment switch
// without reviving the terminal-RAG path.
func (e *Engine) synthesizeKnowledgeQAFromLedger(ctx context.Context, userMsg string) (string, bool) {
	answer, report, ok := e.repairKnowledgeAnswerWithProof(ctx, userMsg, false)
	if !ok {
		return "", false
	}
	e.emitSearchKnowledgeCitationTrace(report)
	e.retractKnowledgeHardBlock()
	return answer, true
}

// synthesizeOnBudgetExceeded writes a final grounded answer from the evidence
// SearchKnowledge already gathered this turn, for the PR2 budget policy: when
// the per-turn token budget is exhausted but evidence is in hand, deliver an
// answer grounded on it rather than discarding the turn for a bare
// "请简化问题". Returns ("", false) when nothing was retrieved or the evidence
// can't produce an answer with a valid claim proof — the caller then keeps the budget
// refusal (the "no evidence → refuse, never fabricate" guard).
//
// Independent of disciplinedKnowledgeQASynthesisOn: that flag gates the NORMAL
// (under-budget) synthesis path, whereas this is a budget-recovery path that
// only fires once the cap is already blown. The repair answer and its proof are
// produced in one call, so an already-paid valid result is not discarded by a
// post-call budget check.
func (e *Engine) synthesizeOnBudgetExceeded(ctx context.Context, userMsg string) (string, bool) {
	return e.synthesizeKnowledgeAnswerAfterBudget(ctx, userMsg)
}
