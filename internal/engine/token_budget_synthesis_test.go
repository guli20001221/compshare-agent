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

	got, ok := eng.synthesizeOnBudgetExceeded(context.Background(), "vllm 显存不足怎么办")
	require.True(t, ok, "evidence in hand + over budget must synthesize a grounded answer, not refuse")
	assert.Contains(t, got, "max-model-len")
	assert.NotContains(t, got, "[1]", "the [n] marker is stripped for display")
}

func TestSynthesizeOnBudgetExceeded_NoEvidenceReturnsFalse(t *testing.T) {
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: `{"answer":"凭空捏造 [1]。"}`}}}, &mockExecutor{}, nil)
	eng.maxTokensPerTurn = 50000
	_, ok := eng.synthesizeOnBudgetExceeded(context.Background(), "q")
	assert.False(t, ok, "with no evidence in hand the budget path must refuse, never fabricate")
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
