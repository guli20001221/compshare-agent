package observability

import "testing"

// TestMergeRetrievalTrace pins the per-turn retrieval merge: a forced SearchKnowledge
// first hop that retrieved evidence must remain observable even when the agent
// re-queries with a miss later in the same turn. Otherwise the latest retrieval
// would erase the evidence-producing activity from the turn trace.
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

func TestMergeRetrievalTraceRetainsRemoteUnavailableSignal(t *testing.T) {
	hits := RetrievalTrace{Enabled: true, KBVersion: "kb.v1", Hits: 1}
	unavailable := RetrievalTrace{Enabled: true, Unavailable: true, FailureReason: "mcp_timeout"}

	for _, traces := range [][2]RetrievalTrace{{hits, unavailable}, {unavailable, hits}} {
		got := MergeRetrievalTrace(traces[0], traces[1])
		if got.Hits != 1 {
			t.Fatalf("Hits = %d, want the substantive retrieval", got.Hits)
		}
		if !got.Unavailable || got.FailureReason != "mcp_timeout" {
			t.Fatalf("remote availability signal was lost: %#v", got)
		}
	}
}

func TestMergeRetrievalTraceOverlaysFinalCitations(t *testing.T) {
	current := RetrievalTrace{
		Enabled:      true,
		KBVersion:    "kb.v1",
		QueryRaw:     "billing",
		Hits:         1,
		HybridMode:   "qwen3_rrf",
		RerankerMode: "qwen3-reranker-8b",
		HitItems: []RetrievalHit{{
			ChunkID:    "chunk-a",
			SourceArea: "billing_rule",
			Score:      0.91,
			Kept:       true,
		}},
		Activities: []RetrievalActivity{{ID: "search_1", Query: "billing", Hits: 1}},
		References: []RetrievalReference{{RefID: "1", ChunkID: "chunk-a", Title: "Billing", ActivityIDs: []string{"search_1"}}},
	}
	next := RetrievalTrace{
		Enabled:       true,
		KBVersion:     "kb.v1",
		QueryRaw:      "billing",
		Hits:          1,
		CitedChunkIDs: []string{"chunk-a"},
		CitedRefs:     []RetrievalCitedRef{{RefID: "1", ChunkID: "chunk-a"}},
		References:    []RetrievalReference{{RefID: "1", ChunkID: "chunk-a", Title: "Billing", ActivityIDs: []string{"search_1"}}},
	}

	got := MergeRetrievalTrace(current, next)

	if got.HybridMode != "qwen3_rrf" || got.RerankerMode != "qwen3-reranker-8b" {
		t.Fatalf("merge lost retrieval diagnostics: %#v", got)
	}
	if len(got.HitItems) != 1 || got.HitItems[0].ChunkID != "chunk-a" {
		t.Fatalf("merge lost hit items: %#v", got.HitItems)
	}
	if len(got.CitedRefs) != 1 || got.CitedRefs[0].ChunkID != "chunk-a" {
		t.Fatalf("merge did not overlay cited refs: %#v", got.CitedRefs)
	}
	if len(got.CitedChunkIDs) != 1 || got.CitedChunkIDs[0] != "chunk-a" {
		t.Fatalf("merge did not overlay cited chunk ids: %#v", got.CitedChunkIDs)
	}
}

// TestMergeRetrievalTraceMultiHopCarriesFullHitItems pins the multi-SearchKnowledge
// case: `current` holds only the LAST call's hit (chunk-b, the "latest substantive
// wins" survivor), while the final citation trace `next` spans the whole turn
// (chunk-a from call 1 + chunk-b from call 2). The merge MUST carry next's full
// HitItems/Hits, else the persisted record cites chunk-a but has no hit_items row for
// it — the exact audit-trail inconsistency the citation-persistence feature exists to
// prevent. Fails if MergeRetrievalTrace's citation branch drops next.HitItems.
func TestMergeRetrievalTraceMultiHopCarriesFullHitItems(t *testing.T) {
	current := RetrievalTrace{
		Enabled: true, KBVersion: "kb.v1", QueryRaw: "search_2", Hits: 1,
		HitItems:   []RetrievalHit{{ChunkID: "chunk-b", Kept: true}},
		Activities: []RetrievalActivity{{ID: "search_2", Query: "q2", Hits: 1}},
	}
	next := RetrievalTrace{
		Enabled: true, KBVersion: "kb.v1", Hits: 2,
		HitItems: []RetrievalHit{
			{ChunkID: "chunk-a", Kept: true},
			{ChunkID: "chunk-b", Kept: true},
		},
		Activities:    []RetrievalActivity{{ID: "search_1", Query: "q1", Hits: 1}, {ID: "search_2", Query: "q2", Hits: 1}},
		References:    []RetrievalReference{{RefID: "1", ChunkID: "chunk-a"}, {RefID: "2", ChunkID: "chunk-b"}},
		CitedChunkIDs: []string{"chunk-a"},
		CitedRefs:     []RetrievalCitedRef{{RefID: "1", ChunkID: "chunk-a"}},
	}

	got := MergeRetrievalTrace(current, next)

	if got.Hits != 2 {
		t.Fatalf("merge did not carry full-turn hit count: got %d, want 2", got.Hits)
	}
	haveA := false
	for _, h := range got.HitItems {
		if h.ChunkID == "chunk-a" {
			haveA = true
		}
	}
	if !haveA {
		t.Fatalf("merge lost the cited chunk-a from hit_items (multi-hop regression): %#v", got.HitItems)
	}
	if len(got.Activities) != 2 {
		t.Fatalf("merge did not union both search activities: %#v", got.Activities)
	}
}

func TestMergeRetrievalTraceMultiHopFailureCarriesFullTurnEvidence(t *testing.T) {
	current := RetrievalTrace{
		Enabled: true, QueryRaw: "q2", Hits: 1,
		HitItems:   []RetrievalHit{{ChunkID: "chunk-b", Kept: true}},
		Activities: []RetrievalActivity{{ID: "search_2", Query: "q2", Hits: 1}},
	}
	next := RetrievalTrace{
		Enabled: true, TurnAggregate: true, Hits: 2,
		HitItems: []RetrievalHit{
			{ChunkID: "chunk-a", Kept: true},
			{ChunkID: "chunk-b", Kept: true},
		},
		Activities: []RetrievalActivity{
			{ID: "search_1", Query: "q1", Hits: 1},
			{ID: "search_2", Query: "q2", Hits: 1},
		},
		References: []RetrievalReference{
			{RefID: "1", ChunkID: "chunk-a", ActivityIDs: []string{"search_1"}},
			{RefID: "2", ChunkID: "chunk-b", ActivityIDs: []string{"search_2"}},
		},
	}

	got := MergeRetrievalTrace(current, next)
	if !got.TurnAggregate || got.Hits != 2 || len(got.HitItems) != 2 || len(got.Activities) != 2 {
		t.Fatalf("failed grounding lost full-turn retrieval evidence: %#v", got)
	}
	if len(got.CitedChunkIDs) != 0 {
		t.Fatalf("failed grounding must not invent citations: %#v", got.CitedChunkIDs)
	}
}
