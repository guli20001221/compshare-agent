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

// TestSynthesizeKnowledgeQAFromLedger_CitedAnswerStripped is the core happy path:
// the gathered SearchKnowledge evidence is fed to terminal RAG's disciplined prompt,
// which returns a positionally-cited answer; synthesizeKnowledgeQAFromLedger accepts
// it and returns the display answer with the [n] marker stripped.
func TestSynthesizeKnowledgeQAFromLedger_CitedAnswerStripped(t *testing.T) {
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{
		{Content: "可以把 max-model-len 调小来降低显存占用 [1]。"},
	}}, &mockExecutor{}, nil)
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{disciplinedSynthHit()}

	got, ok := eng.synthesizeKnowledgeQAFromLedger(context.Background(), "vllm 显存不足怎么办")
	require.True(t, ok, "a positionally-cited terminal synthesis should be accepted")
	assert.Equal(t, "可以把 max-model-len 调小来降低显存占用。", got)
	assert.NotContains(t, got, "[1]", "the [n] marker is stripped for display")
}

func TestSynthesizeKnowledgeQAFromLedger_NoHitsReturnsFalse(t *testing.T) {
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "随便答 [1]。"}}}, &mockExecutor{}, nil)
	// No searchKnowledgeHitsThisTurn → nothing to synthesize from.
	_, ok := eng.synthesizeKnowledgeQAFromLedger(context.Background(), "q")
	assert.False(t, ok)
}

// TestSynthesizeKnowledgeQAFromLedger_UncitedRefused proves the fallback contract:
// when terminal's disciplined synthesis itself cannot land a citation (both the
// first call and its cite-harder retry come back uncited), answerWithRetrievedEvidence
// returns the canned refusal with a reason, and synthesizeKnowledgeQAFromLedger
// returns false — so the caller keeps the existing cite-retry / refusal (never worse).
func TestSynthesizeKnowledgeQAFromLedger_UncitedRefused(t *testing.T) {
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{
		{Content: "这是没有任何编号引用的回答。"},
		{Content: "重试后仍然没有编号引用。"},
	}}, &mockExecutor{}, nil)
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{disciplinedSynthHit()}

	_, ok := eng.synthesizeKnowledgeQAFromLedger(context.Background(), "q")
	assert.False(t, ok, "an uncited synthesis must not be accepted as disciplined recovery")
}

// TestSynthesizeKnowledgeQAFromLedger_CodeReproductionAccepted pins the leak-guard
// removal: the disciplined synthesis mirrors terminal RAG, which runs NO no-raw-leak
// check. A how-to answer that reproduces a command/code snippet verbatim from the
// evidence (the legitimate answer for a code probe like DDP) is ACCEPTED, not rejected
// as a leak — the over-strict prose leak guard was the cause of the agent loop's
// code-heavy over-refusal that terminal does not have.
func TestSynthesizeKnowledgeQAFromLedger_CodeReproductionAccepted(t *testing.T) {
	hit := disciplinedSynthHit()
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{
		{Content: hit.Chunk.Content + " [1]"},
	}}, &mockExecutor{}, nil)
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{hit}

	got, ok := eng.synthesizeKnowledgeQAFromLedger(context.Background(), "q")
	assert.True(t, ok, "verbatim code/command reproduction is the answer, not a leak — accepted like terminal")
	assert.NotContains(t, got, "[1]", "the [n] marker is stripped for display")
}
