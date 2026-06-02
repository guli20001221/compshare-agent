package knowledge

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildEvidenceLedgerOmitsRawChunkContent(t *testing.T) {
	chunk := KBChunk{
		ChunkID: "runbook-port-001",
		Title:   "Service port reachability",
		Content: "For service ports, first verify the instance is Running, then compare exposed software ports.",
	}

	ledger := BuildEvidenceLedger("webui port does not open", []RetrievalHit{{
		Chunk: chunk,
		Score: 0.95,
		Kept:  true,
	}}, 3)

	require.False(t, ledger.Empty())
	require.Len(t, ledger.Items, 1)
	assert.Equal(t, "runbook-port-001", ledger.Items[0].ChunkID)
	assert.Contains(t, ledger.Items[0].Summary, "Service port reachability")

	raw, err := json.Marshal(ledger)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "runbook-port-001")
	assert.Contains(t, string(raw), "Service port reachability")
	assert.NotContains(t, string(raw), "For service ports, first verify")
}

func TestValidateNoRawEvidenceLeakCatchesChunkBody(t *testing.T) {
	chunk := KBChunk{
		ChunkID: "runbook-port-001",
		Title:   "Service port reachability",
		Content: "For service ports, first verify the instance is Running, then compare exposed software ports.",
	}
	hits := []RetrievalHit{{Chunk: chunk, Score: 0.95, Kept: true}}

	assert.NoError(t, ValidateNoRawEvidenceLeak(
		"Evidence runbook-port-001 says to inspect service reachability next.",
		hits,
	))
	err := ValidateNoRawEvidenceLeak(
		"For service ports, first verify the instance is Running, then compare exposed software ports.",
		hits,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runbook-port-001")
}
