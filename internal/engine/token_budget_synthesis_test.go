package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Evidence already retrieved by the Agent may still produce a verified answer
// after the ordinary turn budget is exhausted. With no evidence, the budget
// refusal remains fail closed.
func TestSynthesizeOnBudgetExceeded_DeliversFromEvidenceOverBudget(t *testing.T) {
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{
		{Content: `{"answer":"可以把 max-model-len 调小来降低显存占用[1]。"}`, Usage: llm.TokenUsage{TotalTokens: 60000}},
	}}, &mockExecutor{}, nil)
	eng.maxTokensPerTurn = 50000
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{keptVLLMHit()}

	candidate, ok := eng.synthesizeOnBudgetExceeded(context.Background(), "vllm 显存不足怎么办")
	require.True(t, ok, "evidence in hand + over budget must synthesize a grounded answer, not refuse")
	assert.Contains(t, candidate, "[1]", "the unaccepted candidate keeps its proof marker for final validation")
	assert.Empty(t, eng.sessionState.VerifiedEvidence, "a candidate is not durable before the final gateway passes")
	got := eng.acceptBudgetSynthesis("vllm 显存不足怎么办", candidate)
	assert.Contains(t, got, "max-model-len")
	assert.NotContains(t, got, "[1]", "the [n] marker is stripped for display")
	assert.NotEmpty(t, eng.sessionState.VerifiedEvidence)
}

func TestSynthesizeOnBudgetExceeded_NoEvidenceReturnsFalse(t *testing.T) {
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: `{"answer":"凭空捏造 [1]。"}`}}}, &mockExecutor{}, nil)
	eng.maxTokensPerTurn = 50000
	_, ok := eng.synthesizeOnBudgetExceeded(context.Background(), "q")
	assert.False(t, ok, "with no evidence in hand the budget path must refuse, never fabricate")
}

func TestTerminalEvidenceGatewayStillRunsAfterTokenCap(t *testing.T) {
	gateway := &mockLLM{responses: []llm.ChatResponse{{Content: `{"decision":"retrieve","reason":"evidence_insufficient"}`}}}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetEvidenceGatewayClient(gateway)
	eng.maxTokensPerTurn = 50000
	eng.turnTokensConsumed = 60000
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{keptVLLMHit()}

	got, passed := eng.gateTerminalKnowledgeAnswer(context.Background(), "vllm 显存不足怎么办", "把 max-model-len 调大。", true)
	assert.False(t, passed)
	assert.Contains(t, got, "无法可靠确认")
	assert.Len(t, gateway.calls, 1, "verification remains available after the Agent generation cap")
	assert.Zero(t, eng.evidenceCorrectionCountThisTurn, "terminal recovery cannot start another correction round")
}

func TestTerminalEvidenceGatewayNeverPublishesToolProtocolMarkup(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetEvidenceGatewayClient(&mockLLM{})

	got, passed := eng.gateTerminalKnowledgeAnswer(context.Background(), "重置密码",
		`<｜DSML｜invoke name="RequestResetPassword">{"Password":"Secret123!"}`, true)
	assert.False(t, passed)
	assert.Equal(t, malformedToolProtocolReply, got)
	assert.NotContains(t, got, "Secret123")
}

// The main ReAct exit refuses any non-normal finish_reason rather than persist
// or act on a partial response (engine.go's OutputIncomplete check). This
// recovery path makes its own separate LLM call and must hold the same line:
// a length-stopped synthesis is not a complete, groundable answer.
//
// The JSON here is deliberately WELL-FORMED and parses cleanly — a truncated
// generation does not reliably produce invalid JSON (the model can be cut off
// right after closing the object), so this must be caught by the finish_reason
// check itself, not incidentally by a parse failure. A malformed-JSON fixture
// would pass even with the production check deleted, and prove nothing.
func TestSynthesizeOnBudgetExceeded_LengthStoppedResponseRefuses(t *testing.T) {
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{
		{Content: `{"answer":"可以"}`, StopReason: "length", Usage: llm.TokenUsage{TotalTokens: 60000}},
	}}, &mockExecutor{}, nil)
	eng.maxTokensPerTurn = 50000
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{keptVLLMHit()}

	got, ok := eng.synthesizeOnBudgetExceeded(context.Background(), "vllm 显存不足怎么办")
	assert.False(t, ok, "a length-stopped synthesis response must not be accepted as a complete answer")
	assert.Empty(t, got)
	assert.Equal(t, 60000, eng.turnTokensConsumed, "the truncated call was still paid for and must still be counted")
}
