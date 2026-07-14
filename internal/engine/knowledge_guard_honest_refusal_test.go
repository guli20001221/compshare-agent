package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// There are TWO reasons a knowledge turn can end without an answer, and they are not the
// same reason. Telling the user the wrong one is not a wording nit — it sends them away
// from a question the platform can actually answer.
//
//	no evidence      -> "当前知识库未覆盖该问题"   TRUE. Nothing was retrieved. Nothing to say.
//	evidence, no answer -> the KB DID cover it. We retrieved the material and could not
//	                       write a grounded answer from it. Saying "未覆盖" here is false.
//
// The production replay behind this guard's repair (1454 turns,
// docs/plans/2026-07-13-program-a-amnesia-fix-results.md) found the honest arm fired ZERO
// times: every 「知识库未覆盖」 a user ever saw was shown over evidence we were holding. The
// repair rescued most of them by re-deriving an answer. These two tests pin the exit the
// repair CANNOT rescue — and pin that fixing it did not corrupt the arm that was true.

// The arm that must keep saying "未覆盖", because there it is the truth. This is the
// negative control: without it, "make the message honest" could be satisfied by replacing
// the string everywhere, trading one false message for another.
func TestKnowledgeGuard_WithNoEvidenceTheKBRefusalIsTheHonestOne(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.searchKnowledgeRanThisTurn = true
	eng.searchKnowledgeHitsThisTurn = nil // the relevance floor dropped everything
	eng.knowledgeQAAgentLoopThisTurn = true

	got := eng.repairOrRefuseKnowledgeSynthesis(context.Background(),
		"某个知识库里根本没有的问题", ragNoEvidenceReply)

	assert.Equal(t, ragNoEvidenceReply, got,
		"retrieval genuinely returned nothing, so 「当前知识库未覆盖该问题」 is TRUE here and must stand — "+
			"the honest-message fix must not launder this arm into a softer one it has not earned")
	assert.True(t, isKnowledgeRefusal(got))
}

// The arm that was lying. Evidence was retrieved and every repair rung failed.
func TestKnowledgeGuard_WithEvidenceHeldTheRefusalMustNotBlameTheKB(t *testing.T) {
	SetDisciplinedKnowledgeQASynthesisEnabled(true)
	defer SetDisciplinedKnowledgeQASynthesisEnabled(false)

	// Every rung comes back uncited, so no repair can land: rung 1's cite check rejects it,
	// and rung 2's ValidateGroundedCitations rejects it. The refusal is unavoidable — the
	// only question is what we TELL the user.
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{
		{Content: "把 max-model-len 调小就行。"},
		{Content: "把 max-model-len 调小就行。"},
		{Content: "把 max-model-len 调小就行。"},
		{Content: "把 max-model-len 调小就行。"},
	}}, &mockExecutor{}, nil)
	eng.searchKnowledgeRanThisTurn = true
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{disciplinedSynthHit()}
	eng.searchKnowledgeLedgerThisTurn = knowledge.EvidenceLedger{Items: []knowledge.EvidenceItem{
		{ChunkID: "ext-vllm-oom-001", Title: "vLLM 降显存"},
	}}
	eng.knowledgeQAAgentLoopThisTurn = true

	got := eng.repairOrRefuseKnowledgeSynthesis(context.Background(),
		"vllm 显存不足怎么办", "把 max-model-len 调小就行。")

	require.NotContains(t, got, "max-model-len",
		"precondition: no repair landed, so this really is a refusal — otherwise the test proves nothing")

	assert.NotContains(t, got, "知识库未覆盖",
		"we RETRIEVED the vLLM chunk and showed it to the model. Telling this user the knowledge base "+
			"does not cover their question is the exact opposite of what happened, and it sends them "+
			"away from a question the KB answers")
	assert.Equal(t, ragUngroundableReply, got,
		"say what actually happened: we found the material and could not write a grounded answer from it")

	// The essential property that must survive the wording change.
	assert.True(t, isKnowledgeRefusal(got),
		"it is STILL a refusal. isKnowledgeRefusal gates the cite contract, the synthesis accept check, "+
			"and the retry logic — an honest message that stops matching would silently promote this "+
			"turn to an 'answer' everywhere it is consulted")
}
