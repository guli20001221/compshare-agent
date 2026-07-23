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
