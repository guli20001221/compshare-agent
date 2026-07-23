package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withGapDrivenRetrieval flips the experiment arm for one test and restores it,
// so an ordering change can never leave the flag on for the rest of the package.
func withGapDrivenRetrieval(t *testing.T, enabled bool) {
	t.Helper()
	previous := gapDrivenRetrievalEnabled
	SetGapDrivenRetrievalEnabled(enabled)
	t.Cleanup(func() { SetGapDrivenRetrievalEnabled(previous) })
}

// TestGapDrivenRetrievalIsInertByDefault is the gate that matters most: the arm
// ships off, so a turn must be indistinguishable from one taken before the code
// existed — same retrievals, same tool result, no follow-up field.
func TestGapDrivenRetrievalIsInertByDefault(t *testing.T) {
	require.False(t, GapDrivenRetrievalEnabled(), "the Go-package default must stay off")

	eng, retriever := planningEngineWithConversation(t,
		`{"answer_question":"实例关机后还会产生哪些费用","search_queries":["关机后计费规则","数据盘关机是否计费"]}`,
		twoHitResults())
	out := eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "关机后还收什么"}, noopStep)

	assert.Len(t, retriever.calls, 2, "with the arm off the planner's fan-out still runs up front")
	assert.NotContains(t, out, "follow_up", "the default arm's tool result must carry no affordance")
}

// TestGapDrivenRetrievalWithholdsTheUpfrontFanout covers the first half of the
// arm: the planner still resolves the question, but only the first query is
// spent, leaving budget for a hop chosen after seeing evidence.
func TestGapDrivenRetrievalWithholdsTheUpfrontFanout(t *testing.T) {
	withGapDrivenRetrieval(t, true)

	eng, retriever := planningEngineWithConversation(t,
		`{"answer_question":"实例关机后还会产生哪些费用","search_queries":["关机后计费规则","数据盘关机是否计费"]}`,
		twoHitResults())
	_ = eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "关机后还收什么"}, noopStep)

	require.Len(t, retriever.calls, 1, "only the first planned query may be spent up front")
	assert.Equal(t, "关机后计费规则", retriever.calls[0].question)
	assert.Equal(t, "实例关机后还会产生哪些费用", eng.resolvedKnowledgeQuestionThisTurn,
		"reference resolution is orthogonal to fan-out and must survive")
	assert.Equal(t, 1, eng.searchKnowledgeQueriesThisTurn)
}

// TestGapDrivenRetrievalOffersAFollowUpOnThinEvidence covers the second half:
// an agent that is not told a second hop exists will not take one.
func TestGapDrivenRetrievalOffersAFollowUpOnThinEvidence(t *testing.T) {
	withGapDrivenRetrieval(t, true)

	eng, _ := planningEngineWithConversation(t,
		`{"answer_question":"实例关机后还会产生哪些费用","search_queries":["关机后计费规则"]}`,
		twoHitResults())
	out := eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "关机后还收什么"}, noopStep)

	assert.Contains(t, out, "follow_up")
	assert.Contains(t, out, "更具体的问题", "the affordance must ask for a narrower query, not a repeat")
}

// TestGapDrivenRetrievalNeverOffersAHopTheEngineWouldRefuse guards the failure
// mode that would make the arm look worse than it is: suggesting a follow-up
// after the budget is gone costs the agent a whole round to discover the
// refusal, and that round would be scored against the arm.
func TestGapDrivenRetrievalNeverOffersAHopTheEngineWouldRefuse(t *testing.T) {
	withGapDrivenRetrieval(t, true)

	eng, _ := planningEngineWithConversation(t,
		`{"answer_question":"实例关机后还会产生哪些费用","search_queries":["关机后计费规则"]}`,
		twoHitResults())
	eng.searchKnowledgeCallsThisTurn = maxSearchKnowledgeCallsPerTurn - 1

	out := eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "关机后还收什么"}, noopStep)

	assert.NotContains(t, out, "follow_up",
		"the last permitted call must not advertise a hop the next call would refuse")
}
