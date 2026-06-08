package knowledge

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func ledgerWith(ids ...string) EvidenceLedger {
	items := make([]EvidenceItem, 0, len(ids))
	for _, id := range ids {
		items = append(items, EvidenceItem{ChunkID: id, Title: id + " title"})
	}
	return EvidenceLedger{Items: items}
}

// TestValidateGroundedCitations_KnownCitationGrounds is the happy path: an answer
// citing a ledger ChunkID resolves it and satisfies the cite-or-refuse contract.
func TestValidateGroundedCitations_KnownCitationGrounds(t *testing.T) {
	ledger := ledgerWith("ext-vllm-oom-001", "w0-pricing-003")
	r := ValidateGroundedCitations("把 max-model-len 调小即可省显存 [[ext-vllm-oom-001]]。", ledger)
	assert.True(t, r.HasCitation)
	assert.Equal(t, []string{"ext-vllm-oom-001"}, r.CitedChunkIDs)
	assert.Empty(t, r.UnknownCitations)
	assert.True(t, r.Grounded(), "a known citation grounds the answer")
}

// TestValidateGroundedCitations_UnknownCitationFabricated is the anti-fabrication
// gate: a [[id]] marker not present in the ledger is a fabricated citation and
// fails the contract even though a marker is present.
func TestValidateGroundedCitations_UnknownCitationFabricated(t *testing.T) {
	ledger := ledgerWith("ext-vllm-oom-001")
	r := ValidateGroundedCitations("随便编的结论 [[ext-does-not-exist-999]]。", ledger)
	assert.False(t, r.HasCitation, "an unknown id does not count as a real citation")
	assert.Equal(t, []string{"ext-does-not-exist-999"}, r.UnknownCitations)
	assert.False(t, r.Grounded(), "a fabricated citation must fail the contract")
}

// TestValidateGroundedCitations_MixedKnownAndUnknownFails proves a single
// fabricated citation sinks an otherwise-cited answer — a partially hallucinated
// answer cannot ride in on the strength of one valid citation.
func TestValidateGroundedCitations_MixedKnownAndUnknownFails(t *testing.T) {
	ledger := ledgerWith("ext-vllm-oom-001")
	r := ValidateGroundedCitations("结论A [[ext-vllm-oom-001]] 结论B [[fake-002]]。", ledger)
	assert.True(t, r.HasCitation)
	assert.Equal(t, []string{"ext-vllm-oom-001"}, r.CitedChunkIDs)
	assert.Equal(t, []string{"fake-002"}, r.UnknownCitations)
	assert.False(t, r.Grounded(), "any unknown citation fails, even with a valid one present")
}

// TestValidateGroundedCitations_NoCitationIsNotGrounded pins that an uncited answer
// is NOT grounded. Whether an uncited answer is an acceptable refusal is decided by
// the engine gate (only an explicit canned refusal is cite-exempt) — deliberately
// NOT here, so a substantive answer containing a hedge phrase cannot self-exempt.
func TestValidateGroundedCitations_NoCitationIsNotGrounded(t *testing.T) {
	ledger := ledgerWith("ext-vllm-oom-001")
	r := ValidateGroundedCitations("可以把 max-model-len 调小来省显存。", ledger)
	assert.False(t, r.HasCitation)
	assert.Empty(t, r.UnknownCitations)
	assert.False(t, r.Grounded(), "uncited substantive answer is not grounded")
}

// TestValidateGroundedCitations_OverBracketedMarkerStillResolves proves an
// over-bracketed [[[id]]] still resolves to the inner chunk_id (so the validator
// classifies it correctly) — the strip residue is handled by StripCiteMarkers.
func TestValidateGroundedCitations_OverBracketedMarkerStillResolves(t *testing.T) {
	ledger := ledgerWith("ext-a-1")
	r := ValidateGroundedCitations("结论 [[[ext-a-1]]]。", ledger)
	assert.Equal(t, []string{"ext-a-1"}, r.CitedChunkIDs)
	assert.Empty(t, r.UnknownCitations)
	assert.True(t, r.Grounded())
}

// TestValidateGroundedCitations_NewlineInMarkerFailsClosed proves a marker whose id
// is split across a newline does not match (no newline-bearing id), failing closed.
func TestValidateGroundedCitations_NewlineInMarkerFailsClosed(t *testing.T) {
	ledger := ledgerWith("ext-vllm-oom-001")
	r := ValidateGroundedCitations("结论 [[ext-vllm\noom-001]]。", ledger)
	assert.False(t, r.HasCitation)
	assert.Empty(t, r.CitedChunkIDs)
}

// TestValidateGroundedCitations_DedupesAndTrims pins dedupe + whitespace-tolerant
// matching so repeated citations and [[ id ]] padding do not distort the report.
func TestValidateGroundedCitations_DedupesAndTrims(t *testing.T) {
	ledger := ledgerWith("ext-a-1", "ext-b-2")
	r := ValidateGroundedCitations("A [[ext-a-1]] 再说 A [[ ext-a-1 ]] 然后 B [[ext-b-2]]。", ledger)
	assert.Equal(t, []string{"ext-a-1", "ext-b-2"}, r.CitedChunkIDs, "deduped, first-occurrence order, padding trimmed")
	assert.Empty(t, r.UnknownCitations)
	assert.True(t, r.Grounded())
}

// TestValidateGroundedCitations_IgnoresPositionalAndIncidentalBrackets proves the
// double-bracket scheme does not collide with the terminal-RAG [n] markers or with
// incidental single-bracket prose — those are not treated as citations at all.
func TestValidateGroundedCitations_IgnoresPositionalAndIncidentalBrackets(t *testing.T) {
	ledger := ledgerWith("ext-a-1")
	r := ValidateGroundedCitations("见步骤 [1] 与 [注意] 部分。", ledger)
	assert.False(t, r.HasCitation, "[1] and [注意] are not [[chunk_id]] citations")
	assert.Empty(t, r.CitedChunkIDs)
	assert.Empty(t, r.UnknownCitations)
}

// TestValidateGroundedCitations_EmptyAnswer guards the degenerate input.
func TestValidateGroundedCitations_EmptyAnswer(t *testing.T) {
	r := ValidateGroundedCitations("   ", ledgerWith("ext-a-1"))
	assert.False(t, r.HasCitation)
	assert.False(t, r.Grounded())
}

// TestStripCiteMarkers_RemovesMarkersAndTidies proves the user-facing reply has the
// machine-citation markers removed with the same cosmetic cleanup the [n] strip
// applies, while preserving newlines (markdown structure).
func TestStripCiteMarkers_RemovesMarkersAndTidies(t *testing.T) {
	assert.Equal(t, "把 max-model-len 调小即可省显存。",
		StripCiteMarkers("把 max-model-len 调小即可省显存 [[ext-vllm-oom-001]]。"))
	assert.Equal(t, "方案A、方案B。",
		StripCiteMarkers("方案A [[a-1]]、方案B [[b-2]]。"))
	assert.Equal(t, "line1\nline2",
		StripCiteMarkers("line1 [[a-1]]\nline2 [[b-2]]"))
	assert.Equal(t, "", StripCiteMarkers(""))
	assert.Equal(t, "no markers here", StripCiteMarkers("no markers here"))
	// Over-bracketed markers are consumed whole — no orphan "[]"/"[[]]" residue.
	assert.Equal(t, "把它调小即可。", StripCiteMarkers("把它调小即可 [[[ext-a-1]]]。"))
	assert.Equal(t, "ok", StripCiteMarkers("ok [[[[ext-a-1]]]]"))
}
