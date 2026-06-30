package knowledge

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

func ledgerWith(ids ...string) EvidenceLedger {
	items := make([]EvidenceItem, 0, len(ids))
	for i, id := range ids {
		items = append(items, EvidenceItem{RefID: strconv.Itoa(i + 1), ChunkID: id, Title: id + " title"})
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

// TestValidateGroundedCitations_ToleratesSpacedBrackets covers the 2026-06-08
// raw-synthesis finding: flash reliably cites the right chunk_id but sometimes spaces
// the bracket pairs — "[ [id] ]" / "[[ id ]]" / "[ [ id ] ]" — which a strict \[\[
// missed and wrongly refused a correctly-grounded answer (a perfect 226601 answer was
// false-refused for "[ [w0-init_failure-…] ]"). Spaces/tabs between brackets must
// still resolve; the markers must still strip; incidental non-numeric prose brackets
// stay ignored (the numbered [n] scheme is covered separately).
func TestValidateGroundedCitations_ToleratesSpacedBrackets(t *testing.T) {
	ledger := ledgerWith("w0-init_failure-001", "ext-a-1")
	for _, ans := range []string{
		"实例已欠费，请支付订单 [ [w0-init_failure-001] ]。",
		"实例已欠费 [[ w0-init_failure-001 ]]。",
		"实例已欠费 [ [ w0-init_failure-001 ] ]。",
	} {
		r := ValidateGroundedCitations(ans, ledger)
		assert.True(t, r.Grounded(), "spaced brackets must still ground: %q", ans)
		assert.Equal(t, []string{"w0-init_failure-001"}, r.CitedChunkIDs, ans)
	}
	assert.Equal(t, "实例已欠费，请支付订单。",
		StripCiteMarkers("实例已欠费，请支付订单 [ [w0-init_failure-001] ]。"))
	assert.False(t, ValidateGroundedCitations("见 [注意] 部分。", ledger).HasCitation,
		"non-numeric incidental brackets are not citations")
}

// TestValidateGroundedCitations_PositionalNumberedCitations covers the numbered [n]
// scheme: flash emits a simple [1]/[2] far more reliably than echoing a long
// [[chunk_id]] (and terminal RAG already uses [n]). A known [n] resolves through the
// turn-scoped RefID; legacy ledgers without RefID still fall back to item order. An unknown [n] is a failed citation;
// [[chunk_id]] and [n] compose; the markers strip for display.
func TestValidateGroundedCitations_PositionalNumberedCitations(t *testing.T) {
	ledger := ledgerWith("ext-a-1", "ext-b-2") // [1]->ext-a-1, [2]->ext-b-2

	r := ValidateGroundedCitations("方案A [1]，方案B [2]。", ledger)
	assert.True(t, r.Grounded(), "in-range [n] grounds the answer")
	assert.Equal(t, []string{"ext-a-1", "ext-b-2"}, r.CitedChunkIDs)

	r2 := ValidateGroundedCitations("见步骤 [9]。", ledger)
	assert.False(t, r2.HasCitation, "out-of-range [n] does not ground")
	assert.Equal(t, []string{"9"}, r2.UnknownCitations, "unknown [n] must fail the citation contract")

	r3 := ValidateGroundedCitations("X [[ext-b-2]] 和 Y [1]。", ledger)
	assert.True(t, r3.Grounded())
	assert.ElementsMatch(t, []string{"ext-b-2", "ext-a-1"}, r3.CitedChunkIDs, "[[chunk_id]] and [n] compose")

	assert.Equal(t, "方案A，方案B。", StripCiteMarkers("方案A [1]，方案B [2]。"))
}

func TestValidateGroundedCitations_RefIDPreferredOverPosition(t *testing.T) {
	ledger := EvidenceLedger{Items: []EvidenceItem{
		{RefID: "2", ChunkID: "chunk-b"},
		{RefID: "1", ChunkID: "chunk-a"},
	}}
	r := ValidateGroundedCitations("结论来自 A [1]。", ledger)
	assert.True(t, r.Grounded())
	assert.Equal(t, []string{"chunk-a"}, r.CitedChunkIDs, "numeric markers resolve ref_id before legacy position")
}

func TestValidateGroundedCitations_UnknownRefIDDoesNotFallbackToPosition(t *testing.T) {
	ledger := EvidenceLedger{Items: []EvidenceItem{
		{RefID: "10", ChunkID: "chunk-a"},
		{RefID: "11", ChunkID: "chunk-b"},
	}}
	r := ValidateGroundedCitations("结论来自未知编号 [2]。", ledger)
	assert.False(t, r.HasCitation, "agentic ledgers must not resolve unknown ref_id by item position")
	assert.Equal(t, []string{"2"}, r.UnknownCitations)
	assert.False(t, r.Grounded())
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

// TestValidateGroundedCitations_IgnoresIncidentalBrackets proves that non-numeric
// prose brackets are ignored while unknown numeric refs fail closed.
func TestValidateGroundedCitations_IgnoresIncidentalBrackets(t *testing.T) {
	ledger := ledgerWith("ext-a-1") // only [1] is in range
	r := ValidateGroundedCitations("见 [注意] 部分，另见 [9]。", ledger)
	assert.False(t, r.HasCitation, "[注意] is not a citation and [9] is unknown")
	assert.Empty(t, r.CitedChunkIDs)
	assert.Equal(t, []string{"9"}, r.UnknownCitations)
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
