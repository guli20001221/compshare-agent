package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestZoneConvergence_NoLegacyChainInProduction is the S6 architecture gate: the
// deleted zone-resolution chain must not creep back into production code. It scans
// the non-test Go sources of the three packages the convergence touched
// (workflow, engine, zones) and fails if any forbidden token reappears — so a future
// change that re-adds a per-zone param map, a zone alias table, or a second
// zone-matching LLM is caught at test time rather than in review.
//
// Zones are resolved from ONE per-turn deployment.ZoneCatalogSnapshot; nothing in
// production reads a ZoneIds/ZoneRegionIds/ZoneIsPods/ZoneDescribes map, calls the
// old engine chain, uses the deleted zones matcher primitives, or rebuilds a second
// catalog inside executeWorkflow (which now takes the snapshot as a parameter).
// Tests may still name these tokens (fixtures, this gate), so only non-test sources
// are scanned.
func TestZoneConvergence_NoLegacyChainInProduction(t *testing.T) {
	forbidden := []string{
		// The four legacy per-zone maps (quoted so prose/comments never false-trip):
		// production resolves every zone field from the snapshot record instead.
		`"ZoneIds"`, `"ZoneRegionIds"`, `"ZoneIsPods"`, `"ZoneDescribes"`,
		// The deleted engine-side chain and its second, zone-matching LLM.
		"applyCreateZoneResolution", "resolveRequestedZone", "matchZoneLLM",
		"deployZoneAliases", "deploymentZonePlacement",
		// The deleted workflow-side bridge helpers.
		"legacyZonePlacementFromParams", "guidedZoneID", "guidedZoneRegionID",
		"guidedZoneIsPod", "zoneOptionLabel", "zoneDescribeMapFromParams",
		// The deleted zones-package matcher primitives.
		"MatchSystemPrompt", "ParseDecision", "func Mentions",
		// The deleted self-query builder and the option that threaded a prebuilt
		// catalog: executeWorkflow now REQUIRES the snapshot as a parameter and never
		// builds its own, so a second catalog can no longer come into being here.
		"zoneCatalogSnapshotForAction", "withPrebuiltZoneCatalog",
	}

	for _, dir := range []string{".", "../engine", "../zones"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read dir %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			text := string(src)
			for _, tok := range forbidden {
				if strings.Contains(text, tok) {
					t.Errorf("%s reintroduces forbidden zone-convergence token %q — production must resolve zones from the ZoneCatalogSnapshot, not the deleted chain", path, tok)
				}
			}
		}
	}
}
