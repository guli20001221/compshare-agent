package engine

import (
	"fmt"
	"testing"

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
	require.Equal(t, "chunk-15", got.Items[14].ChunkID, "later searches must not be hidden by the durable-memory cap")
}
