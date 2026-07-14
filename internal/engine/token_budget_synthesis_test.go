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

// PR2 budget policy: when the per-turn token cap is blown but evidence was
// already retrieved this turn, deliver a grounded answer from it instead of a
// bare "请简化问题"; refuse ONLY when there is no groundable answer in hand.
// These tests pin both halves — at the choke-point primitive
// (answerWithRetrievedEvidence) and at the wrapper the ReAct-loop budget gates
// call (synthesizeOnBudgetExceeded).

// TestAnswerWithRetrievedEvidence_OverBudgetCitedDelivered: a cited grounded
// answer is delivered even when the answerer call itself tipped the turn over
// cap. Pre-PR2 the budget gate sat ABOVE the cite check and discarded the
// answer for tokenBudgetExceededMessage — exactly the false-refusal of a simple
// question in a long (prior-text-heavy) session. Contract now: refusedReason=""
// (delivered), the cited content flows through, and NO budget hard-block fires
// (the turn answered, it was not blocked).
func TestAnswerWithRetrievedEvidence_OverBudgetCitedDelivered(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "可以把 max-model-len 调小来降低显存占用 [1]。", Usage: llm.TokenUsage{TotalTokens: 60000}},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.maxTokensPerTurn = 50000
	var hardBlocks []observability.EngineHardBlockTrace
	eng.SetHardBlockObserver(func(tr observability.EngineHardBlockTrace) { hardBlocks = append(hardBlocks, tr) })

	evidences, err := evidencesFromRetrievalHits([]knowledge.RetrievalHit{disciplinedSynthHit()}, knowledge.NormalizeQuery("vllm 显存不足"))
	require.NoError(t, err)
	require.NotEmpty(t, evidences)

	reply, _, refusedReason, _, aerr := eng.answerWithRetrievedEvidence(context.Background(), "vllm 显存不足", evidences, false, nil)
	require.NoError(t, aerr)
	assert.Equal(t, "", refusedReason,
		"a cited answer must be delivered over budget, not converted to the budget refusal")
	assert.Contains(t, reply, "max-model-len", "the grounded answer already in hand must flow through")
	assert.NotEqual(t, tokenBudgetExceededMessage, reply)
	assert.Len(t, mock.calls, 1, "the cited first answer returns immediately — no retry")
	assert.Empty(t, hardBlocks,
		"delivering a grounded answer must NOT emit a budget hard-block (the turn was not blocked)")
}

// TestAnswerWithRetrievedEvidence_OverBudgetUncitedRefusesAndSuppressesRetry:
// the protection half. Over budget with a first answer that is neither an
// honest refusal nor cited, there is nothing groundable to deliver and we must
// NOT spend the cite-harder retry. Contract: budget refusal + "token_budget"
// reason, exactly one LLM call (retry suppressed), one budget hard-block.
func TestAnswerWithRetrievedEvidence_OverBudgetUncitedRefusesAndSuppressesRetry(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "没有任何编号引用的回答。", Usage: llm.TokenUsage{TotalTokens: 60000}},
		// Would be returned IF a retry happened — it must NOT, so it never surfaces.
		{Content: "重试后才补上的引用 [1]。"},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.maxTokensPerTurn = 50000
	var hardBlocks []observability.EngineHardBlockTrace
	eng.SetHardBlockObserver(func(tr observability.EngineHardBlockTrace) { hardBlocks = append(hardBlocks, tr) })

	evidences, err := evidencesFromRetrievalHits([]knowledge.RetrievalHit{disciplinedSynthHit()}, knowledge.NormalizeQuery("q"))
	require.NoError(t, err)

	reply, _, refusedReason, _, aerr := eng.answerWithRetrievedEvidence(context.Background(), "q", evidences, false, nil)
	require.NoError(t, aerr)
	assert.Equal(t, tokenBudgetExceededMessage, reply,
		"over budget with an uncited first answer and no room to retry must refuse, never fabricate")
	assert.Equal(t, "token_budget", refusedReason)
	assert.Len(t, mock.calls, 1, "the cite-harder retry must be suppressed once over budget — no further tokens")
	require.Len(t, hardBlocks, 1, "the budget refusal must emit exactly one budget hard-block")
	assert.Equal(t, observability.HardBlockTriggerTokenBudget, hardBlocks[0].TriggeredBy)
}

// TestAnswerWithRetrievedEvidence_UnderBudgetUncitedStillRetries guards that the
// PR2 change did NOT remove the normal cite-harder retry — when we are NOT over
// budget, an uncited first answer must still trigger the second call (here the
// retry lands the citation and is delivered).
func TestAnswerWithRetrievedEvidence_UnderBudgetUncitedStillRetries(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{Content: "没有编号引用的初答。", Usage: llm.TokenUsage{TotalTokens: 1000}},
		{Content: "重试后补上引用 [1]。", Usage: llm.TokenUsage{TotalTokens: 1000}},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.maxTokensPerTurn = 50000 // 2000 total stays well under cap

	evidences, err := evidencesFromRetrievalHits([]knowledge.RetrievalHit{disciplinedSynthHit()}, knowledge.NormalizeQuery("q"))
	require.NoError(t, err)

	reply, _, refusedReason, _, aerr := eng.answerWithRetrievedEvidence(context.Background(), "q", evidences, false, nil)
	require.NoError(t, aerr)
	assert.Equal(t, "", refusedReason, "under budget, the cite-harder retry must still run and its cited answer delivered")
	assert.Contains(t, reply, "重试后补上引用")
	assert.Len(t, mock.calls, 2, "under budget the retry must still fire")
}

// TestSynthesizeOnBudgetExceeded_DeliversFromEvidenceOverBudget: the wrapper the
// ReAct-loop budget gates use. Evidence in hand + over budget => a grounded,
// citation-stripped answer (ok=true), not a refusal.
func TestSynthesizeOnBudgetExceeded_DeliversFromEvidenceOverBudget(t *testing.T) {
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{
		{Content: `{"answer":"可以把 max-model-len 调小来降低显存占用。","supported":true,"claims":[{"answer_quote":"可以把 max-model-len 调小来降低显存占用","chunk_id":"ext-vllm-oom-001","evidence_quote":"把 max-model-len 设置得小一些就能显著降低显存占用"}],"unsupported":[]}`, Usage: llm.TokenUsage{TotalTokens: 60000}},
	}}, &mockExecutor{}, nil)
	eng.maxTokensPerTurn = 50000
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{disciplinedSynthHit()}

	got, ok := eng.synthesizeOnBudgetExceeded(context.Background(), "vllm 显存不足怎么办")
	require.True(t, ok, "evidence in hand + over budget must synthesize a grounded answer, not refuse")
	assert.Contains(t, got, "max-model-len")
	assert.NotContains(t, got, "[1]", "the [n] marker is stripped for display")
}

// TestSynthesizeOnBudgetExceeded_NoEvidenceReturnsFalse: the "no evidence =>
// refuse" guard. With nothing retrieved this turn the wrapper returns false so
// the caller keeps the budget refusal — we never fabricate an answer with no
// evidence to stand on.
func TestSynthesizeOnBudgetExceeded_NoEvidenceReturnsFalse(t *testing.T) {
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "凭空捏造 [1]。"}}}, &mockExecutor{}, nil)
	eng.maxTokensPerTurn = 50000
	// searchKnowledgeHitsThisTurn left empty — no evidence in hand.
	_, ok := eng.synthesizeOnBudgetExceeded(context.Background(), "q")
	assert.False(t, ok, "with no evidence in hand the budget path must refuse, never fabricate")
}
