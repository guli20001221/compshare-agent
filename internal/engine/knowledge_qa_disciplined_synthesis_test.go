package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func disciplinedSynthHit() knowledge.RetrievalHit {
	return knowledge.RetrievalHit{Kept: true, Score: 90, Chunk: knowledge.KBChunk{
		ChunkID:   "ext-vllm-oom-001",
		KBVersion: "merged", // real SearchKnowledge hits carry kb_version; NewEvidence requires it
		Title:     "vLLM 降显存",
		Content:   "把 max-model-len 设置得小一些就能显著降低显存占用，过长的上下文会占用更多 KV cache。",
	}}
}

func TestDisciplinedKnowledgeQASynthesisEnabled_DefaultOff(t *testing.T) {
	assert.False(t, DisciplinedKnowledgeQASynthesisEnabled(), "disciplined synthesis must default off (byte-identical)")
}

// The live repair seam accepts only an answer carrying an exact, server-checked
// proof against the same EvidenceLedger.
func TestSynthesizeKnowledgeQAFromLedger_ProofCarryingAnswerAccepted(t *testing.T) {
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{
		{Content: `{"answer":"可以把 max-model-len 调小来降低显存占用。","supported":true,"claims":[{"answer_quote":"可以把 max-model-len 调小来降低显存占用","chunk_id":"ext-vllm-oom-001","evidence_quote":"把 max-model-len 设置得小一些就能显著降低显存占用"}],"unsupported":[]}`},
	}}, &mockExecutor{}, nil)
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{disciplinedSynthHit()}
	eng.searchKnowledgeLedgerThisTurn = knowledge.BuildSubstantiveEvidenceLedger("完整问题", eng.searchKnowledgeHitsThisTurn, 3, 0)

	got, ok := eng.synthesizeKnowledgeQAFromLedger(context.Background(), "vllm 显存不足怎么办")
	require.True(t, ok)
	assert.Equal(t, "可以把 max-model-len 调小来降低显存占用。", got)
}

func TestSynthesizeKnowledgeQAFromLedger_NoHitsReturnsFalse(t *testing.T) {
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: `{"answer":"随便答","supported":true}`}}}, &mockExecutor{}, nil)
	// No searchKnowledgeHitsThisTurn → nothing to synthesize from.
	_, ok := eng.synthesizeKnowledgeQAFromLedger(context.Background(), "q")
	assert.False(t, ok)
}

func TestSynthesizeKnowledgeQAFromLedger_UnsupportedAnswerRefused(t *testing.T) {
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{
		{Content: `{"answer":"猜测的答案","supported":false,"claims":[],"unsupported":["证据不足"]}`},
	}}, &mockExecutor{}, nil)
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{disciplinedSynthHit()}
	eng.searchKnowledgeLedgerThisTurn = knowledge.BuildSubstantiveEvidenceLedger("q", eng.searchKnowledgeHitsThisTurn, 3, 0)

	_, ok := eng.synthesizeKnowledgeQAFromLedger(context.Background(), "q")
	assert.False(t, ok)
}

func TestSynthesizeKnowledgeQAFromLedger_UnknownChunkRefused(t *testing.T) {
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{
		{Content: `{"answer":"可以把 max-model-len 调小来降低显存占用。","supported":true,"claims":[{"answer_quote":"可以把 max-model-len 调小来降低显存占用","chunk_id":"unknown","evidence_quote":"把 max-model-len 设置得小一些"}],"unsupported":[]}`},
	}}, &mockExecutor{}, nil)
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{disciplinedSynthHit()}
	eng.searchKnowledgeLedgerThisTurn = knowledge.BuildSubstantiveEvidenceLedger("q", eng.searchKnowledgeHitsThisTurn, 3, 0)

	_, ok := eng.synthesizeKnowledgeQAFromLedger(context.Background(), "q")
	assert.False(t, ok)
}

func TestSynthesizeKnowledgeQAFromLedger_RawEvidenceDumpRefused(t *testing.T) {
	hit := disciplinedSynthHit()
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{
		{Content: `{"answer":"` + hit.Chunk.Content + `","supported":true,"claims":[{"answer_quote":"` + hit.Chunk.Content + `","chunk_id":"ext-vllm-oom-001","evidence_quote":"` + hit.Chunk.Content + `"}],"unsupported":[]}`},
	}}, &mockExecutor{}, nil)
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{hit}
	eng.searchKnowledgeLedgerThisTurn = knowledge.BuildSubstantiveEvidenceLedger("q", eng.searchKnowledgeHitsThisTurn, 3, 0)

	_, ok := eng.synthesizeKnowledgeQAFromLedger(context.Background(), "q")
	assert.False(t, ok, "the agent-loop repair cannot bypass the raw-evidence leak guard")
}
