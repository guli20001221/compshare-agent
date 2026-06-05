package intent

import (
	"testing"

	"github.com/compshare-agent/internal/observability"
)

// TestRouteStatusBindsToRealizedTier is the real-data binding for
// observability.DeriveRealizedTier. That function (and its own package-local
// test) must hardcode the cutover_status STRING values, because intent imports
// observability (handler.go / trace_projection.go / shadow.go) so observability
// cannot import intent back — the import cycle forbids referencing
// intent.RouteStatus* from inside the derivation. This test lives on the
// intent side, where the constants ARE in scope, and feeds every RouteStatus
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
// RouteStatus block must appear below. Adding a RouteStatus without adding a
// row here is the failure mode to watch for in review.
func TestRouteStatusBindsToRealizedTier(t *testing.T) {
	cases := []struct {
		status RouteStatus
		want   string
	}{
		{RouteStatusNone, ""},
		{RouteStatusDispatched, observability.RealizedTierFast},
		{RouteStatusSelectionRequired, observability.RealizedTierFast},
		{RouteStatusDispatchedRetrieval, observability.RealizedTierKnowledge},
		{RouteStatusFallbackInvalid, ""},
		{RouteStatusFallbackLowConfidence, ""},
		{RouteStatusFallbackIneligible, ""},
		{RouteStatusFallbackUnresolvedTarget, ""},
		{RouteStatusFallbackTimeWindow, ""},
		{RouteStatusFailureAfterTool, ""},
		{RouteStatusFallbackRetrievalMiss, ""},
		{RouteStatusFallbackRetrievalDisabled, ""},
	}
	for _, tc := range cases {
		rec := observability.TraceRecord{
			Planner: observability.PlannerTrace{RouteStatus: string(tc.status)},
		}
		if got := rec.DeriveRealizedTier(); got != tc.want {
			t.Errorf("RouteStatus %q: DeriveRealizedTier() = %q, want %q",
				tc.status, got, tc.want)
		}
	}
}
