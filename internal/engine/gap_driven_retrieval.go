package engine

import "fmt"

// Gap-driven retrieval is the experiment arm for "form the second query from
// what the first retrieval actually returned, instead of predicting both before
// seeing anything".
//
// Today the multi-turn planner decides the whole retrieval plan up front: it
// emits one to maxKnowledgePlanQueries queries in a single call, before any
// evidence exists. That is a prediction about where the gaps will be. The
// alternative — retrieve once, look at what came back, then target what is
// missing — is what the agentic-RAG literature calls the corrective loop, and it
// only became reachable at all once the per-turn budget stopped being consumed
// by the planner's own fan-out.
//
// This is deliberately NOT a new mechanism. The agent could always call
// SearchKnowledge again; the flag changes only two things:
//
//  1. the planner is capped to a single query, so the budget is spent on
//     evidence the agent has seen rather than on predictions, and
//  2. a thin result carries an explicit follow-up affordance naming the
//     remaining budget, because an agent that does not know a second hop is
//     available will not take one.
//
// Default OFF and boot-only, frozen the same way the other engine flags are:
// the Go-package default stays off so unit tests are unaffected, and turning it
// on is an eval-gated decision, not a code change.
var gapDrivenRetrievalEnabled bool

// SetGapDrivenRetrievalEnabled freezes the flag at boot. Never call it per turn:
// changing retrieval strategy mid-session would make one conversation's evidence
// come from two different policies.
func SetGapDrivenRetrievalEnabled(enabled bool) {
	gapDrivenRetrievalEnabled = enabled
}

// GapDrivenRetrievalEnabled reports the frozen setting.
func GapDrivenRetrievalEnabled() bool {
	return gapDrivenRetrievalEnabled
}

// narrowPlanForGapDrivenRetrieval keeps only the first planned query. The
// planner still runs — reference resolution ("浏览器里呢", "关机后还收什么") is
// what makes the query standalone and is orthogonal to how many queries follow
// it. Only the up-front fan-out is withheld.
func narrowPlanForGapDrivenRetrieval(plan knowledgeQueryPlan) knowledgeQueryPlan {
	if !gapDrivenRetrievalEnabled || len(plan.SearchQueries) <= 1 {
		return plan
	}
	plan.SearchQueries = plan.SearchQueries[:1]
	return plan
}

// followUpAffordance describes the remaining retrieval budget to the agent after
// a thin result. Returns "" when the arm is off, when the evidence looks
// sufficient, or when nothing is left to spend — an affordance that names a hop
// the engine would refuse is worse than none, because the agent burns a round
// discovering the refusal.
func (e *Engine) followUpAffordance(itemCount int) string {
	if !gapDrivenRetrievalEnabled || itemCount >= gapDrivenSufficientItems {
		return ""
	}
	callsLeft := maxSearchKnowledgeCallsPerTurn - e.searchKnowledgeCallsThisTurn
	queriesLeft := maxRetrievalQueriesPerTurn - e.searchKnowledgeQueriesThisTurn
	if callsLeft <= 0 || queriesLeft <= 0 {
		return ""
	}
	return fmt.Sprintf(
		"本次检索到 %d 条证据，可能不足以完整回答。你还可以再检索 %d 次。"+
			"如果要再检索，请针对上面证据没有覆盖到的那一部分提出更具体的问题，不要重复同一个问题。",
		itemCount, callsLeft)
}

// gapDrivenSufficientItems is the point above which a result is treated as
// probably answerable and no follow-up is suggested. Three, because the ledger
// is already relevance-filtered so surviving items are not padding. It is a
// nudge threshold, not a quality judgement: the agent may search again below or
// above it, and nothing downstream reads this number.
const gapDrivenSufficientItems = 3
