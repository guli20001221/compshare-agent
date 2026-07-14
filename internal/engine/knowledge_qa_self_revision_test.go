package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKQASelfRevisionFlagDefaultOffAndToggle(t *testing.T) {
	assert.False(t, KQASelfRevisionEnabled(), "Go-package default must stay off so unit tests are unaffected")
	SetKQASelfRevisionEnabled(true)
	assert.True(t, KQASelfRevisionEnabled())
	SetKQASelfRevisionEnabled(false)
	assert.False(t, KQASelfRevisionEnabled())
}

func TestKQASelfRevisionFlagAddsDirectnessInsideProofCarryingRepair(t *testing.T) {
	previous := KQASelfRevisionEnabled()
	SetKQASelfRevisionEnabled(true)
	t.Cleanup(func() { SetKQASelfRevisionEnabled(previous) })

	hit := disciplinedSynthHit()
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: `{"answer":"可以把 max-model-len 调小来降低显存占用。","supported":true,"claims":[{"answer_quote":"可以把 max-model-len 调小来降低显存占用","chunk_id":"ext-vllm-oom-001","evidence_quote":"把 max-model-len 设置得小一些就能显著降低显存占用"}],"unsupported":[]}`}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{hit}
	eng.searchKnowledgeLedgerThisTurn = knowledge.BuildSubstantiveEvidenceLedger("vllm 显存不足怎么办", eng.searchKnowledgeHitsThisTurn, 3, 0)

	answer, _, ok := eng.repairKnowledgeAnswerWithProof(context.Background(), "那怎么办", false)

	require.True(t, ok)
	assert.Contains(t, answer, "max-model-len")
	require.Len(t, mock.calls, 1, "directness guidance must not add a second revision call")
	assert.Contains(t, mock.calls[0].Messages[0].Content, "证据已经明确支持的结论要直接说清")
}
