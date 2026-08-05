package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func priorPricingLedger() knowledge.EvidenceLedger {
	return knowledge.EvidenceLedger{
		Query: "4090 一小时多少钱",
		Items: []knowledge.EvidenceItem{{
			ChunkID: "w0-pricing-4090",
			Title:   "计费概览",
			Snippet: "RTX 4090 按量计费为每小时 2.00 元。",
		}},
	}
}

// The budget / round-ceiling / LLM-error recovery path GENERATES the answer the
// user reads. It must therefore write only from evidence THIS turn gathered.
//
// It did not. The ledger came from knowledgeLedgerForVerification, which merges
// prior verified evidence in, so a turn that retrieved nothing at all still
// produced a confident answer built from a chunk fetched for a different question
// several turns earlier — and then stored that answer, copying the same chunk into
// a second entry with a fresh timestamp, which is how a chunk retrieved once stops
// ever ageing out of the verifiedKnowledgeMaxTurns window.
//
// The exits are not hypothetical: replaying production traffic they produced the
// 处理轮次超限 cluster (M106/M110/M115/M143) and the 180s-timeout cluster
// (M095/M118/M148), and 51 of 127 replayed sessions carry verified_knowledge.
func TestBudgetRecoveryRefusesOnPriorEvidenceAlone(t *testing.T) {
	// A live LLM response is scripted deliberately. If the guard regresses, the
	// test must fail on the ANSWER being produced, not on an empty mock queue.
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{
		{Content: `{"answer":"RTX 4090 按量计费每小时 2.00 元[1]。"}`},
	}}, &mockExecutor{}, nil)
	eng.maxTokensPerTurn = 50000
	eng.rememberVerifiedKnowledge("4090 一小时多少钱", "每小时 2 元。", priorPricingLedger())

	require.Len(t, eng.sessionState.VerifiedKnowledge, 1,
		"premise: the prior entry must exist, or this test proves nothing")
	require.NotEmpty(t, eng.knowledgeLedgerForVerification("那包月呢").Items,
		"premise: the verifier can still see it — this test is about the GENERATOR, not the verifier")
	require.Empty(t, eng.searchKnowledgeHitsThisTurn, "premise: this turn retrieved nothing")
	require.Empty(t, eng.readResponseEvidenceThisTurn, "premise: this turn read nothing")

	got, ok := eng.synthesizeOnBudgetExceeded(context.Background(), "那包月呢")
	assert.False(t, ok,
		"a turn that retrieved nothing must keep the canned refusal, not answer a monthly-billing "+
			"question out of an hourly-pricing chunk fetched for a different question")
	assert.Empty(t, got)
	assert.Len(t, eng.sessionState.VerifiedKnowledge, 1,
		"and it must not re-stamp the prior chunk into a second entry, which would reset its age")
}

// The other half: with evidence of its own, the recovery still delivers. This is
// the behaviour the guard must not have cost — pinned separately so a fix that
// simply disabled the path cannot pass.
func TestBudgetRecoveryStillDeliversFromThisTurnsEvidence(t *testing.T) {
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{
		{Content: `{"answer":"可以把 max-model-len 调小来降低显存占用[1]。"}`},
	}}, &mockExecutor{}, nil)
	eng.maxTokensPerTurn = 50000
	eng.rememberVerifiedKnowledge("4090 一小时多少钱", "每小时 2 元。", priorPricingLedger())
	eng.searchKnowledgeHitsThisTurn = []knowledge.RetrievalHit{keptVLLMHit()}

	got, ok := eng.synthesizeOnBudgetExceeded(context.Background(), "vllm 显存不足怎么办")
	require.True(t, ok, "evidence in hand + over budget must still synthesize rather than refuse")
	assert.Contains(t, got, "max-model-len")
}

// A grounded answer stores THIS turn's evidence. Storing the merged ledger the
// verifier used would copy prior chunks forward under a new timestamp on every
// grounded turn, making them permanent.
func TestStoredEvidenceIsThisTurnsOnly(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.rememberVerifiedKnowledge("4090 一小时多少钱", "每小时 2 元。", priorPricingLedger())
	eng.searchKnowledgeLedgerThisTurn = knowledge.EvidenceLedger{
		Query: "包月怎么算",
		Items: []knowledge.EvidenceItem{{
			ChunkID: "w0-pricing-monthly",
			Title:   "包月计费",
			Snippet: "包月按 30 天整月计费。",
		}},
	}

	stored := eng.currentTurnEvidenceLedger("包月怎么算")
	require.Len(t, stored.Items, 1, "premise: this turn retrieved exactly one chunk")
	assert.Equal(t, "w0-pricing-monthly", stored.Items[0].ChunkID)

	ids := make([]string, 0, 2)
	for _, item := range eng.knowledgeLedgerForVerification("包月怎么算").Items {
		ids = append(ids, item.ChunkID)
	}
	assert.ElementsMatch(t, []string{"w0-pricing-monthly", "w0-pricing-4090"}, ids,
		"the VERIFIER still sees both — narrowing what is generated from and stored "+
			"must not narrow what an answer is checked against")
}
