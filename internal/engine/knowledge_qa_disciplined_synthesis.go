package engine

import "context"

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
// This is the budget-recovery form of the same single repair path and only
// fires once the cap is already blown. The repair answer and its proof are
// produced in one call, so an already-paid valid result is not discarded by a
// post-call budget check.
func (e *Engine) synthesizeOnBudgetExceeded(ctx context.Context, userMsg string) (string, bool) {
	return e.synthesizeKnowledgeAnswerAfterBudget(ctx, userMsg)
}
