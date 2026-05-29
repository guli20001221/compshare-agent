package intent

import (
	"testing"

	"github.com/compshare-agent/internal/observability"
)

// TestCutoverStatusBindsToRealizedTier is the real-data binding for
// observability.DeriveRealizedTier. That function (and its own package-local
// test) must hardcode the cutover_status STRING values, because intent imports
// observability (handler.go / trace_projection.go / shadow.go) so observability
// cannot import intent back — the import cycle forbids referencing
// intent.CutoverStatus* from inside the derivation. This test lives on the
// intent side, where the constants ARE in scope, and feeds every CutoverStatus
// value through the real derivation. If a constant's VALUE is renamed in
// handler.go, the literal inside DeriveRealizedTier stops matching and this test
// fails — closing the silent-desync gap the package-local test can't.
//
// Signal here is cutover_status ALONE (no retrieval hits, no tool calls), so the
// expected tier is what the status-name branch yields; fallbacks and
// failure_after_tool fall through to the secondary signals, which are absent, so
// they resolve to "" (unknown) — never default-agent.
//
// Exhaustiveness is by explicit enumeration: every const in the handler.go
// CutoverStatus block must appear below. Adding a CutoverStatus without adding a
// row here is the failure mode to watch for in review.
func TestCutoverStatusBindsToRealizedTier(t *testing.T) {
	cases := []struct {
		status CutoverStatus
		want   string
	}{
		{CutoverStatusNone, ""},
		{CutoverStatusDispatched, observability.RealizedTierFast},
		{CutoverStatusSelectionRequired, observability.RealizedTierFast},
		{CutoverStatusDispatchedRetrieval, observability.RealizedTierKnowledge},
		{CutoverStatusFallbackInvalid, ""},
		{CutoverStatusFallbackLowConfidence, ""},
		{CutoverStatusFallbackIneligible, ""},
		{CutoverStatusFallbackUnresolvedTarget, ""},
		{CutoverStatusFallbackTimeWindow, ""},
		{CutoverStatusFailureAfterTool, ""},
		{CutoverStatusFallbackRetrievalMiss, ""},
		{CutoverStatusFallbackRetrievalDisabled, ""},
	}
	for _, tc := range cases {
		rec := observability.TraceRecord{
			Planner: observability.PlannerTrace{CutoverStatus: string(tc.status)},
		}
		if got := rec.DeriveRealizedTier(); got != tc.want {
			t.Errorf("CutoverStatus %q: DeriveRealizedTier() = %q, want %q",
				tc.status, got, tc.want)
		}
	}
}
