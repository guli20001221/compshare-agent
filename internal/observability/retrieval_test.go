package observability

import "testing"

// TestDeriveRefusalType pins the four-state mapping, including the load-bearing
// split: "no_evidence" → corpus_gap vs all_below_floor decided by FloorDroppedAll.
func TestDeriveRefusalType(t *testing.T) {
	cases := []struct {
		name            string
		refusedReason   string
		floorDroppedAll bool
		unavailable     bool
		want            string
	}{
		{"clean turn is unclassified", "", false, false, ""},
		{"no_evidence + corpus empty → corpus_gap", "no_evidence", false, false, RefusalTypeCorpusGap},
		{"no_evidence + floor dropped all → all_below_floor", "no_evidence", true, false, RefusalTypeAllBelowFloor},
		{"weak_evidence → synthesis_refused", "weak_evidence", false, false, RefusalTypeSynthesisRefused},
		{"refusal → synthesis_refused", "refusal", false, false, RefusalTypeSynthesisRefused},
		{"retry_no_cite → synthesis_refused", "retry_no_cite", false, false, RefusalTypeSynthesisRefused},
		{"wrong_domain → wrong_domain", "wrong_domain", false, false, RefusalTypeWrongDomain},
		{"remote MCP outage is not a corpus gap", "no_evidence", false, true, ""},
		// Infra failures are not knowledge-coverage refusals (outcome.terminated_by
		// owns them), so they stay unclassified even if a floor drop co-occurred.
		{"token_budget is unclassified", "token_budget", true, false, ""},
		{"llm_error is unclassified", "llm_error", false, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			trace := RetrievalTrace{RefusedReason: c.refusedReason, FloorDroppedAll: c.floorDroppedAll, Unavailable: c.unavailable}
			if got := trace.DeriveRefusalType(); got != c.want {
				t.Fatalf("DeriveRefusalType(reason=%q, floorDroppedAll=%v) = %q, want %q",
					c.refusedReason, c.floorDroppedAll, got, c.want)
			}
		})
	}
}

// TestRetrievalTraceFloorFields_Observed verifies the new floor/source fields
// participate in the observed gate (so a trace carrying only one of them still
// emits) without disturbing SHA stability for a fully-zero trace.
func TestRetrievalTraceFloorFields_Observed(t *testing.T) {
	if traceRetrievalObserved(RetrievalTrace{}) {
		t.Fatal("zero RetrievalTrace must not be observed")
	}
	for _, tr := range []RetrievalTrace{
		{RefusalType: RefusalTypeCorpusGap},
		{FloorDroppedAll: true},
		{FloorValue: 0.5},
	} {
		if !traceRetrievalObserved(tr) {
			t.Fatalf("trace %#v must be observed", tr)
		}
	}
}
