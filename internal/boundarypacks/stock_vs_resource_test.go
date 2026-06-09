package boundarypacks

import (
	"strings"
	"testing"
)

// stockRule is the exact stock-vs-resource directive (verbatim from the planner
// base prompt it was extracted from). Tests reference it as the contract text.
const stockRule = "Inventory availability questions like whether a GPU model has stock, is available, is sold out, or has data-center inventory are not resource_info. resource_info is only for the user's own CompShare instances. Platform stock questions should emit stock_availability."

// stockVsResourceBoundaryPackSHA256Baseline pins the stock-vs-resource pack's
// directive content INDEPENDENTLY of the full planner-prompt hash. A change to
// the pack's directive text must bump this AND be justified in the commit — the
// pack is a router-time classification contract.
const stockVsResourceBoundaryPackSHA256Baseline = "a76bbd9c1335b1e457fd07dc75a0ba78d7a10e35464e101c2720a28b4de56a3d"

func TestStockVsResourceBoundaryPack_MatchesBaselineSHA(t *testing.T) {
	got, ok := PackSHA256(BoundaryPackStockVsResource)
	if !ok {
		t.Fatal("stock_vs_resource pack not found")
	}
	if got != stockVsResourceBoundaryPackSHA256Baseline {
		t.Errorf("stock_vs_resource pack SHA drifted.\n  baseline: %s\n  current:  %s\n"+
			"If intentional, update stockVsResourceBoundaryPackSHA256Baseline and justify in the commit.",
			stockVsResourceBoundaryPackSHA256Baseline, got)
	}
}

// TestBoundaryPromptFragments_OrderStable pins the projection order. A new pack
// must be added to boundaryPackOrder (and here) to project; order never relies
// on map iteration or append happenstance.
func TestBoundaryPromptFragments_OrderStable(t *testing.T) {
	want := []BoundaryPackID{BoundaryPackStockVsResource}
	got := Order()
	if len(got) != len(want) {
		t.Fatalf("boundaryPackOrder = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("boundaryPackOrder[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// Every ordered ID must resolve to a defined pack (no dangling order entry).
	for _, id := range got {
		if _, ok := PackDirectives(id); !ok {
			t.Errorf("boundaryPackOrder lists %q but no pack is defined for it", id)
		}
	}
}

// TestBoundaryPromptFragments_RendersStockRuleOnce asserts the projection
// contains the stock-vs-resource rule exactly once and verbatim.
func TestBoundaryPromptFragments_RendersStockRuleOnce(t *testing.T) {
	rendered := strings.Join(BoundaryPromptFragments(), "\n")
	if n := strings.Count(rendered, stockRule); n != 1 {
		t.Errorf("boundary fragments contain the stock-vs-resource rule %d times, want exactly 1", n)
	}
}
