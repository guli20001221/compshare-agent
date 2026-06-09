package intent

import (
	"testing"

	"github.com/compshare-agent/internal/routing"
)

// TestRouteRequiredToolsSubsetOfRouteToolSubset closes the route-manifest half of
// the RequiredTools ⊆ ToolSubset invariant: every tool a route declares as
// required must be one the route's own ReAct tool window actually exposes. A
// route whose RequiredTool fell outside its ToolSubset would name a tool the
// planner can never see for that intent — a silent dead-end. The companion
// TestRouteSkills_ReactToolSubsetMatchesIntentToolSubset already pins
// route.ToolSubset == IntentToolSubset(intent); this one pins the orthogonal
// containment, so the two together make "a route only requires tools it grants"
// executable rather than merely true-by-inspection.
func TestRouteRequiredToolsSubsetOfRouteToolSubset(t *testing.T) {
	for _, route := range routing.GeneratedRoutes() {
		if route.IntentLabel == "" {
			continue
		}
		subset := make(map[string]struct{}, len(route.ToolSubset))
		for _, name := range route.ToolSubset {
			subset[name] = struct{}{}
		}
		for _, req := range route.RequiredTools {
			if _, ok := subset[req]; !ok {
				t.Errorf("route %s (intent %s): RequiredTool %q not in ToolSubset %v",
					route.Name, route.IntentLabel, req, route.ToolSubset)
			}
		}
	}
}
