package engine

// buildRAGAnswerStrategyNote is the SINGLE source of the RAG citation rule. Every
// same-model RAG answer-strategy nudge is assembled here, so how we ask the Agent to
// cite can never drift between the two exits.
//
// Two states reach it and pass ONLY their own framing + scope clause; neither
// restates the citation rule:
//   - cite-or-drop retry  (searchKnowledgeCiteRetryNote, knowledge_answer_grounding.go):
//     an answer was already written but left this-turn facts uncited — rewrite it.
//   - budget/round recovery (searchKnowledgeBudgetSynthNote,
//     knowledge_qa_disciplined_synthesis.go): evidence was gathered but no final
//     answer was produced — write it now.
//
// The invariant rule owned here: number-cite every fact drawn from THIS turn's
// evidence ([1], [2] in evidence-item order), and never paste raw evidence verbatim
// (paraphrase). Both outputs then pass the SAME deterministic, fail-open citation
// handling (finalizeAgentLoopKnowledgeAnswer / synthesizeOnBudgetExceeded): resolving
// markers are recorded, all markers are stripped for display, a non-leaking answer
// ships even if uncited, and only a persistent raw-evidence leak is a hard stop.
func buildRAGAnswerStrategyNote(framing, scopeClause string) string {
	return framing +
		`每条来自本轮资料的事实用 [1]、[2] 这样的编号标注（编号对应本轮证据条目的顺序）；` +
		scopeClause +
		`不要整段复制资料原文，用自己的话概括。`
}
