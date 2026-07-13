package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The per-turn token budget exists to stop the NEXT call. It is not a reason to throw away an
// answer that has already been paid for.
//
// This package already states that contract, in so many words, in answerWithRetrievedEvidence:
//
//	"A grounded (cited) first answer ... is delivered regardless of the per-turn token budget
//	 ... The budget only suppresses spending MORE tokens ... never the delivery of an answer
//	 already in hand."
//
// retrySearchKnowledgeCitation was violating it. It gated the call on the budget (correct —
// that gate does the budget's actual work), made the call, and then gated AGAIN on the way out,
// discarding the reply before a single check had looked at it. The tokens were already spent;
// the discard saved nothing and cost the user a cited, leak-free answer.
//
// The two tests below are the two halves of the contract, and they must both hold or the fix is
// just a hole: a call already made is delivered, a call not yet made is suppressed.
func TestRetrySearchKnowledgeCitation_ACallAlreadyPaidForIsNotDiscarded(t *testing.T) {
	const cited = "把 max-model-len 调小可以显著降低显存占用 [1]。"

	// One call, and it tips the turn over the cap all by itself — the realistic shape, since
	// the retry re-sends the whole ReAct history. The reply that comes back is exactly what we
	// asked for: grounded, cited, no raw dump.
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: cited, Usage: llm.TokenUsage{TotalTokens: 60000}},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.maxTokensPerTurn = 50000
	eng.searchKnowledgeRanThisTurn = true
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{disciplinedSynthHit()}
	eng.searchKnowledgeLedgerThisTurn = knowledge.EvidenceLedger{Items: []knowledge.EvidenceItem{
		{ChunkID: "ext-vllm-oom-001", Title: "vLLM 降显存"},
	}}
	eng.knowledgeQAAgentLoopThisTurn = true

	got, ok := eng.retrySearchKnowledgeCitation(context.Background())

	require.True(t, ok,
		"the retry call was made and its tokens spent; discarding the cited answer it returned saves nothing and costs the user the answer")
	assert.Contains(t, got, "max-model-len", "the answer already in hand must flow through")
	assert.NotContains(t, got, "[1]", "the cite marker is a grounding proof, stripped before display")
	assert.Len(t, mock.calls, 1, "exactly the one call that was already gated and paid for")
}

// The other half. Once the cap is ALREADY blown on the way in, the retry must not be made at
// all — this is the gate that does the budget's real work, and removing the redundant one on
// the way out must not weaken it. Without this assertion the fix above could be "achieved" by
// deleting both gates.
func TestRetrySearchKnowledgeCitation_OverBudgetOnEntryMakesNoCall(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "这次调用根本不该发生 [1]。"},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.maxTokensPerTurn = 50000
	eng.turnTokensConsumed = 60000 // the cap was blown before we got here
	eng.searchKnowledgeRanThisTurn = true
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{disciplinedSynthHit()}
	eng.knowledgeQAAgentLoopThisTurn = true

	_, ok := eng.retrySearchKnowledgeCitation(context.Background())

	assert.False(t, ok, "over cap on entry, the retry must not run")
	assert.Empty(t, mock.calls, "the budget's job is to prevent the NEXT call — that job is still done here")
}

// A turn the engine RECOVERED must not be filed as a refusal — no matter who wrote the refusal
// down.
//
// The first version of the retraction asked "did the GUARD block?" and only withdrew then. That
// question is the wrong one, and this is the turn that proves it. The sequence below is
// production's, replayed through the real branches (engine.go: disciplined synthesis at the top
// of the no-tool-calls block, then repairOrRefuseKnowledgeSynthesis when it fails):
//
//	1. disciplined synthesis runs -> answerWithRetrievedEvidence -> the model's first draft is
//	   uncited AND that call blew the per-turn cap -> a REAL token_budget_exceeded hard block
//	   fires and the synthesis gives up;
//	2. the caller falls through to repairOrRefuseKnowledgeSynthesis with the canned refusal;
//	3. the repair re-derives the answer from the ledger and SUCCEEDS. The user gets a correct,
//	   grounded answer.
//
// The block from step 1 is still standing. The recorder keeps the LAST emission per turn, and
// message_recorder turns Hit=true into a DB row with status="blocked" — so this turn shipped a
// correct answer and recorded itself as a refusal. That poisons the exact metric the guard fix
// is judged by, in the exact direction that would make the fix look like it did nothing.
//
// Nothing here is hand-constructed: both hard blocks are emitted by the production branches that
// emit them live.
func TestKnowledgeGuard_ARecoveredTurnIsNotFiledAsBlocked_WhoeverWroteTheBlock(t *testing.T) {
	SetDisciplinedKnowledgeQASynthesisEnabled(true)
	defer SetDisciplinedKnowledgeQASynthesisEnabled(false)

	const uncitedDraft = "把 max-model-len 调小就行。" // no [n]: the cite arm, as flash does live
	const citedAnswer = "把 max-model-len 调小可以显著降低显存占用 [1]。"

	mock := &mockLLM{responses: []llm.ChatResponse{
		// (1) disciplined synthesis, first draft: uncited AND it blows the cap by itself.
		//     answerWithRetrievedEvidence then suppresses its own cite-harder retry and fires
		//     the token-budget hard block. This is the block that must later be withdrawn.
		{Content: uncitedDraft, Usage: llm.TokenUsage{TotalTokens: 60000}},
		// (2) the repair's rung 2 re-derives from the ledger and lands the citation. PR2's
		//     policy delivers it even over cap, because the evidence is in hand.
		{Content: citedAnswer},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.maxTokensPerTurn = 50000
	eng.searchKnowledgeRanThisTurn = true
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{disciplinedSynthHit()}
	eng.searchKnowledgeLedgerThisTurn = knowledge.EvidenceLedger{Items: []knowledge.EvidenceItem{
		{ChunkID: "ext-vllm-oom-001", Title: "vLLM 降显存"},
	}}
	eng.knowledgeQAAgentLoopThisTurn = true

	var traces []observability.EngineHardBlockTrace
	eng.SetHardBlockObserver(func(tr observability.EngineHardBlockTrace) { traces = append(traces, tr) })

	ctx := context.Background()
	const userMsg = "vllm 显存不足怎么办"

	// Step 1 — the engine's own first move on this turn (engine.go, disciplined synthesis).
	_, synthDone := eng.synthesizeKnowledgeQAFromLedger(ctx, userMsg)
	require.False(t, synthDone, "precondition: the over-budget uncited draft must have failed the synthesis")
	require.Len(t, traces, 1, "precondition: the REAL token-budget branch must have fired a hard block")
	require.True(t, traces[0].Hit)
	require.Equal(t, observability.HardBlockCategoryTokenBudget, traces[0].Category,
		"precondition: the standing block is a TOKEN BUDGET block — not the guard's, which is the whole point")

	// Step 2 — exactly what engine.go does when synthDone is false.
	got := eng.repairOrRefuseKnowledgeSynthesis(ctx, userMsg, ragNoEvidenceReply)

	require.NotEqual(t, ragNoEvidenceReply, got, "precondition: the repair must have landed an answer")
	assert.Contains(t, got, "max-model-len", "the user got a real, grounded answer")

	// THE ASSERTION. The turn answered. Whatever block was standing must be withdrawn.
	last := traces[len(traces)-1]
	assert.False(t, last.Hit,
		"the user got a correct answer, so this turn was NOT blocked — leaving the token-budget block standing files a success as a failure and inflates the refusal rate this fix is measured by")
}
