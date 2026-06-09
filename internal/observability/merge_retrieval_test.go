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
		name           string
		current, next  RetrievalTrace
		wantHits       int
		wantKBVersion  string
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
