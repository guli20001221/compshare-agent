package observability

import (
	"testing"

	"github.com/compshare-agent/internal/knowledge/agentic"
)

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

func TestMergeRetrievalTracePreservesAgenticMetadataBackfill(t *testing.T) {
	hits := RetrievalTrace{
		Enabled: true,
		Hits:    1,
		References: []agentic.Reference{{
			RefID:   "1",
			ChunkID: "chunk-a",
		}},
		Activities: []agentic.RetrievalActivity{{ID: "act-1", Query: "q"}},
	}
	citationOnly := RetrievalTrace{
		RefIDScheme:   "turn_1_based",
		CitedRefs:     []string{"1"},
		CitedChunkIDs: []string{"chunk-a"},
	}

	got := MergeRetrievalTrace(hits, citationOnly)

	if got.Hits != 1 {
		t.Fatalf("Hits = %d, want 1", got.Hits)
	}
	if len(got.References) != 1 || got.References[0].ChunkID != "chunk-a" {
		t.Fatalf("References = %#v, want chunk-a", got.References)
	}
	if len(got.Activities) != 1 || got.Activities[0].ID != "act-1" {
		t.Fatalf("Activities = %#v, want act-1", got.Activities)
	}
	if len(got.CitedRefs) != 1 || got.CitedRefs[0] != "1" {
		t.Fatalf("CitedRefs = %#v, want [1]", got.CitedRefs)
	}
	if len(got.CitedChunkIDs) != 1 || got.CitedChunkIDs[0] != "chunk-a" {
		t.Fatalf("CitedChunkIDs = %#v, want [chunk-a]", got.CitedChunkIDs)
	}
}
