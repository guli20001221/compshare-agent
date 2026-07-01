package observability

import "testing"

// TestMergeRetrievalTrace pins the per-turn retrieval merge: a forced SearchKnowledge
// first hop that retrieved evidence must remain observable even when the agent
// re-queries with a miss later in the same turn (the COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP
// case the eval gate must be able to prove). Otherwise the latest retrieval wins, so
// terminal RAG (a single retrieval) is unchanged.
func TestMergeRetrievalTrace(t *testing.T) {
	hits := RetrievalTrace{Enabled: true, KBVersion: "kb.v1", Hits: 3}
	empty := RetrievalTrace{Enabled: true, Hits: 0, RefusedReason: "no_evidence"}
	zero := RetrievalTrace{}

	cases := []struct {
		name          string
		current, next RetrievalTrace
		wantHits      int
		wantKBVersion string
	}{
		{"first retrieval ever (zero -> hits) takes incoming", zero, hits, 3, "kb.v1"},
		{"first retrieval ever (zero -> empty) takes incoming", zero, empty, 0, ""},
		{"forced hits then trailing empty re-query keeps hits", hits, empty, 3, "kb.v1"},
		{"hits then a second substantive retrieval takes latest", hits, RetrievalTrace{Enabled: true, KBVersion: "kb.v2", Hits: 1}, 1, "kb.v2"},
		{"empty then hits takes the hits-bearing one", empty, hits, 3, "kb.v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MergeRetrievalTrace(tc.current, tc.next)
			if got.Hits != tc.wantHits {
				t.Fatalf("Hits = %d, want %d", got.Hits, tc.wantHits)
			}
			if got.KBVersion != tc.wantKBVersion {
				t.Fatalf("KBVersion = %q, want %q", got.KBVersion, tc.wantKBVersion)
			}
		})
	}
}

func TestMergeRetrievalTrace_MergesCitationOnlyTraceIntoHits(t *testing.T) {
	current := RetrievalTrace{
		Enabled:   true,
		KBVersion: "kb.v1",
		Hits:      2,
		HitItems: []RetrievalHit{
			{ChunkID: "chunk-a"},
			{ChunkID: "chunk-b"},
		},
	}
	next := RetrievalTrace{
		CitedChunkIDs: []string{"chunk-b"},
		CitedRefs:     []string{"2"},
		References: []RetrievalReference{
			{RefID: "1", ChunkID: "chunk-a"},
			{RefID: "2", ChunkID: "chunk-b"},
		},
	}

	got := MergeRetrievalTrace(current, next)
	if got.Hits != 2 {
		t.Fatalf("Hits = %d, want 2", got.Hits)
	}
	if got.KBVersion != "kb.v1" {
		t.Fatalf("KBVersion = %q, want kb.v1", got.KBVersion)
	}
	if len(got.CitedChunkIDs) != 1 || got.CitedChunkIDs[0] != "chunk-b" {
		t.Fatalf("CitedChunkIDs = %#v, want [chunk-b]", got.CitedChunkIDs)
	}
	if len(got.CitedRefs) != 1 || got.CitedRefs[0] != "2" {
		t.Fatalf("CitedRefs = %#v, want [2]", got.CitedRefs)
	}
	if len(got.References) != 2 || got.References[1].ChunkID != "chunk-b" {
		t.Fatalf("References = %#v, want ref_id/chunk_id mapping for chunk-a and chunk-b", got.References)
	}
}
