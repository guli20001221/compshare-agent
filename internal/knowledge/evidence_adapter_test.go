package knowledge

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildEvidenceLedgerOmitsRawChunkContent(t *testing.T) {
	chunk := KBChunk{
		ChunkID:    "runbook-port-001",
		SourceType: "runbook",
		Title:      "Service port reachability",
		Content:    "For service ports, first verify the instance is Running, then compare exposed software ports.",
	}

	ledger := BuildEvidenceLedger("webui port does not open", []RetrievalHit{{
		Chunk: chunk,
		Score: 0.95,
		Kept:  true,
	}}, 3)

	require.False(t, ledger.Empty())
	require.Len(t, ledger.Items, 1)
	assert.Equal(t, "runbook-port-001", ledger.Items[0].ChunkID)
	assert.Equal(t, "runbook", ledger.Items[0].SourceType)
	assert.Equal(t, "high", ledger.Items[0].ScoreBucket)
	assert.Contains(t, ledger.Items[0].Summary, "Service port reachability")

	raw, err := json.Marshal(ledger)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "runbook-port-001")
	assert.Contains(t, string(raw), "Service port reachability")
	assert.NotContains(t, string(raw), "For service ports, first verify")
}

func TestMergeEvidenceLedgersKeepsEarlierSearchEvidence(t *testing.T) {
	first := EvidenceLedger{
		Query: "first query",
		Items: []EvidenceItem{{
			ChunkID: "runbook-first",
			Summary: "first safe summary",
		}},
	}
	second := EvidenceLedger{
		Query: "second query",
		Items: []EvidenceItem{{
			ChunkID: "runbook-second",
			Summary: "second safe summary",
		}},
	}

	merged := MergeEvidenceLedgers(first, second, 3)

	assert.Equal(t, "first query | second query", merged.Query)
	require.Len(t, merged.Items, 2)
	assert.Equal(t, "runbook-first", merged.Items[0].ChunkID)
	assert.Equal(t, "runbook-second", merged.Items[1].ChunkID)
}

func TestEchoedEvidenceChunkIDNamesTheCopiedChunk(t *testing.T) {
	chunk := KBChunk{
		ChunkID: "runbook-port-001",
		Title:   "Service port reachability",
		Content: "For service ports, first verify the instance is Running, then compare exposed software ports.",
	}
	hits := []RetrievalHit{{Chunk: chunk, Score: 0.95, Kept: true}}

	assert.Empty(t, EchoedEvidenceChunkID(
		"Evidence runbook-port-001 says to inspect service reachability next.",
		hits,
	), "naming a chunk_id and paraphrasing is not an echo")
	assert.Equal(t, "runbook-port-001", EchoedEvidenceChunkID(
		"For service ports, first verify the instance is Running, then compare exposed software ports.",
		hits,
	), "a verbatim body passage is attributed to its chunk")
}
