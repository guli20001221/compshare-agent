package engine

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/stretchr/testify/require"
)

func TestKnowledgeLedgerForVerificationKeepsEveryCurrentTurnChunk(t *testing.T) {
	eng := NewWithDeps(nil, nil, nil)
	items := make([]knowledge.EvidenceItem, 0, 15)
	for i := 1; i <= 15; i++ {
		items = append(items, knowledge.EvidenceItem{
			ChunkID: fmt.Sprintf("chunk-%02d", i),
			Snippet: fmt.Sprintf("evidence %02d", i),
		})
	}
	eng.searchKnowledgeLedgerThisTurn = knowledge.EvidenceLedger{Query: "q", Items: items}

	got := eng.knowledgeLedgerForVerification("q")

	require.Len(t, got.Items, 15, "the verifier must see the same complete current-turn evidence set as the Agent")
	require.Equal(t, "chunk-15", got.Items[14].ChunkID, "later searches must not be hidden by the persisted-evidence cap")
}

func TestCurrentReadEvidenceLedgerKeepsSnippetWithinGatewayLimit(t *testing.T) {
	eng := NewWithDeps(nil, nil, nil)
	eng.platformReadEvidenceThisTurn = []platformReadEvidence{{
		Capability: "resource_info",
		Reply:      strings.Repeat("实", maxEvidenceGatewayFactRunes+100),
		Envelope:   envelope.Envelope{Kind: envelope.KindResourceInfo},
	}}

	got := eng.currentReadEvidenceLedger("当前还有实例吗")

	require.Len(t, got.Items, 1)
	require.LessOrEqual(t, utf8.RuneCountInString(got.Items[0].Snippet), maxEvidenceGatewayFactRunes)
}
