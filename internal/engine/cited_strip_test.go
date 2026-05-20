package engine

import (
	"testing"

	"github.com/compshare-agent/internal/knowledge"
)

// TestExtractCitedChunkIDsBasicMapping locks the 1-indexed [n] -> hits[n-1]
// mapping that the audit trail relies on. If the prompt assembler ever
// changes its numbering convention (e.g. switches to 0-indexed), this test
// breaks first and surfaces the contract drift before MySQL ingest gets
// silently misaligned chunk_ids.
func TestExtractCitedChunkIDsBasicMapping(t *testing.T) {
	hits := []knowledge.RetrievalHit{
		{Chunk: knowledge.KBChunk{ChunkID: "chunk-A"}},
		{Chunk: knowledge.KBChunk{ChunkID: "chunk-B"}},
		{Chunk: knowledge.KBChunk{ChunkID: "chunk-C"}},
	}
	got := extractCitedChunkIDs("First fact [1]. Second fact [2][3].", hits)
	want := []string{"chunk-A", "chunk-B", "chunk-C"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d; got=%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestExtractCitedChunkIDsDedupesPreservingOrder catches the "LLM cited
// [1] twice" case — the audit list should record chunk-A once at first-
// occurrence position, not duplicate it. Downstream MySQL ingest assumes
// each chunk_id appears at most once per turn.
func TestExtractCitedChunkIDsDedupesPreservingOrder(t *testing.T) {
	hits := []knowledge.RetrievalHit{
		{Chunk: knowledge.KBChunk{ChunkID: "chunk-A"}},
		{Chunk: knowledge.KBChunk{ChunkID: "chunk-B"}},
	}
	got := extractCitedChunkIDs("A [2] then B [1] then again [2] [1].", hits)
	want := []string{"chunk-B", "chunk-A"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d; got=%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestExtractCitedChunkIDsDropsOutOfRange locks that an LLM hallucinating
// citations like [9] when only 3 hits exist does NOT crash and does NOT
// produce empty-string chunk_ids in the audit. The cited contract gate is
// the place that decides whether to coerce the answer; this helper just
// records what's mappable.
func TestExtractCitedChunkIDsDropsOutOfRange(t *testing.T) {
	hits := []knowledge.RetrievalHit{
		{Chunk: knowledge.KBChunk{ChunkID: "chunk-A"}},
		{Chunk: knowledge.KBChunk{ChunkID: "chunk-B"}},
	}
	got := extractCitedChunkIDs("A [1] but also [9] and [0].", hits)
	want := []string{"chunk-A"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("got=%v, want=%v", got, want)
	}
}

// TestExtractCitedChunkIDsReturnsNilWhenNoCitations covers the refusal
// path: ragNoEvidenceReply and similar refusal templates have no [n]
// markers, so the audit field must be nil (not []string{}) so JSON
// omitempty drops it from trace output cleanly.
func TestExtractCitedChunkIDsReturnsNilWhenNoCitations(t *testing.T) {
	hits := []knowledge.RetrievalHit{
		{Chunk: knowledge.KBChunk{ChunkID: "chunk-A"}},
	}
	if got := extractCitedChunkIDs("当前知识库未覆盖该问题,我无法回答。", hits); got != nil {
		t.Fatalf("got=%v, want nil for refusal reply", got)
	}
	if got := extractCitedChunkIDs("", hits); got != nil {
		t.Fatalf("got=%v, want nil for empty answer", got)
	}
}

// TestStripCitationMarkersRemovesAllMarkers locks the cosmetic
// "[n]" -> "" rewrite that the user-facing reply path relies on.
// Multiple markers, adjacent markers, and markers at sentence-end
// boundaries must all be cleanly removed.
func TestStripCitationMarkersRemovesAllMarkers(t *testing.T) {
	in := "首先 [1] 提到 A，然后 [2][3] 又说 B。最后 [1]。"
	out := stripCitationMarkers(in)
	if want := "首先 提到 A，然后 又说 B。最后。"; out != want {
		t.Fatalf("got=%q, want=%q", out, want)
	}
}

// TestStripCitationMarkersCollapsesAdjacentSpaces guards against the
// double-space artifact that would otherwise be visible to the user
// when " [1] " around a marker collapses to "  ". The cosmetic cleanup
// must collapse consecutive ASCII spaces but NOT touch newlines (which
// would break markdown lists / tables that the LLM produces).
func TestStripCitationMarkersCollapsesAdjacentSpaces(t *testing.T) {
	got := stripCitationMarkers("foo [1] bar")
	if got != "foo bar" {
		t.Fatalf("space-collapse failed: got=%q", got)
	}
	multiline := "Line A [1]\n- bullet [2]\nLine C"
	got = stripCitationMarkers(multiline)
	want := "Line A\n- bullet\nLine C"
	if got != want {
		t.Fatalf("newline preservation failed: got=%q, want=%q", got, want)
	}
}

// TestStripCitationMarkersEmptyAndNoCitation locks the no-op behavior on
// strings that don't contain markers — the function must not damage
// refusal templates, canned replies, or already-stripped content.
func TestStripCitationMarkersEmptyAndNoCitation(t *testing.T) {
	if got := stripCitationMarkers(""); got != "" {
		t.Fatalf("empty input mutated: got=%q", got)
	}
	if got := stripCitationMarkers("当前知识库未覆盖该问题,我无法回答。"); got != "当前知识库未覆盖该问题,我无法回答。" {
		t.Fatalf("refusal reply mutated: got=%q", got)
	}
}
