package engine

import (
	"testing"

	"github.com/compshare-agent/internal/knowledge"
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

// A grounded answer stores THIS turn's evidence. Storing the merged ledger the
// verifier used would copy prior chunks forward under a new timestamp on every
// grounded turn, making them permanent.
func TestStoredEvidenceIsThisTurnsOnly(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.rememberVerifiedEvidence("4090 一小时多少钱", priorPricingLedger())
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
