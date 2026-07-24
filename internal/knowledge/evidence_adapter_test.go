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

func TestValidateDiagnosisClaimsRequiresSameTurnEvidence(t *testing.T) {
	ledger := EvidenceLedger{Items: []EvidenceItem{{ChunkID: "runbook-port-001"}}}

	claims, err := ValidateDiagnosisClaims([]DiagnosisClaim{{
		Claim:    "Platform runbook should be checked before endpoint diagnosis.",
		Status:   DiagnosisClaimSupported,
		ChunkIDs: []string{"runbook-port-001"},
	}}, ledger)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	assert.Equal(t, DiagnosisClaimSupported, claims[0].Status)

	_, err = ValidateDiagnosisClaims([]DiagnosisClaim{{
		Claim:    "Unknown evidence is not allowed.",
		Status:   DiagnosisClaimSupported,
		ChunkIDs: []string{"missing"},
	}}, ledger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing")

	claims, err = ValidateDiagnosisClaims([]DiagnosisClaim{{
		Claim:  "Supported without evidence IDs should not discard the whole diagnosis.",
		Status: DiagnosisClaimSupported,
		Reason: "model omitted IDs",
	}}, ledger)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	assert.Equal(t, DiagnosisClaimUnconfirmed, claims[0].Status)
	assert.Contains(t, claims[0].Reason, "downgraded to unconfirmed")

	claims, err = ValidateDiagnosisClaims([]DiagnosisClaim{{
		Claim:  "Instance state can be inferred from read-only API output.",
		Status: DiagnosisClaimInferred,
	}}, ledger)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	assert.Equal(t, DiagnosisClaimInferred, claims[0].Status)

	_, err = ValidateDiagnosisClaims([]DiagnosisClaim{{
		Claim:  "Ambiguous support status is rejected.",
		Status: "maybe",
	}}, ledger)
	require.Error(t, err)
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
	_, err := ValidateDiagnosisClaims([]DiagnosisClaim{{
		Claim:    "The first search evidence remains valid.",
		Status:   DiagnosisClaimSupported,
		ChunkIDs: []string{"runbook-first"},
	}}, merged)
	require.NoError(t, err)
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
