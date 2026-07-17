package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/compshare-agent/internal/deployment"
)

// TestWorkflowZonePlacement_PrefersSnapshotOverLegacyMaps proves the migration:
// when the run carries a zone catalog, the placement comes from that single
// record — NOT from the legacy per-zone maps, which here deliberately disagree.
// In production the two agree (same catalog source); this pins which one wins so
// that removing the maps in S6 changes nothing.
func TestWorkflowZonePlacement_PrefersSnapshotOverLegacyMaps(t *testing.T) {
	snap := deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-bj2-03", Region: "cn-bj2", ZoneID: 6003, AzGroup: 3003, IsPod: true}},
	})
	wfCtx := NewContext(map[string]any{
		// Legacy maps say something DIFFERENT — the snapshot must override them.
		"ZoneIds":       map[string]uint32{"cn-bj2-03": 111},
		"ZoneRegionIds": map[string]uint32{"cn-bj2-03": 222},
		"ZoneIsPods":    map[string]bool{"cn-bj2-03": false},
	})
	wfCtx.ReferenceData.ZoneCatalog = snap

	got := workflowZonePlacement(wfCtx, "cn-bj2-03")

	assert.Equal(t, deployment.ZonePlacement{Zone: "cn-bj2-03", Region: "cn-bj2", ZoneID: 6003, AzGroup: 3003, IsPod: true}, got,
		"the single catalog record wins over the legacy maps")
}

// TestWorkflowZonePlacement_FallsBackToMapsWithoutSnapshot pins the bridge: a run
// with no snapshot (a direct workflow-engine test) still resolves from the maps,
// so the migration does not break the existing suite before S6 removes them.
func TestWorkflowZonePlacement_FallsBackToMapsWithoutSnapshot(t *testing.T) {
	wfCtx := NewContext(map[string]any{
		"ZoneIds":       map[string]uint32{"cn-sh2-02": 2002},
		"ZoneRegionIds": map[string]uint32{"cn-sh2-02": 3002},
		"ZoneIsPods":    map[string]bool{"cn-sh2-02": false},
	})

	got := workflowZonePlacement(wfCtx, "cn-sh2-02")

	assert.Equal(t, uint32(2002), got.ZoneID, "without a snapshot the legacy maps still resolve")
	assert.Equal(t, uint32(3002), got.AzGroup)
	assert.False(t, got.IsPod)
}

// TestNetOptimizerAzGroup_PrefersSnapshot pins the same migration for the
// net-optimizer's az_group source.
func TestNetOptimizerAzGroup_PrefersSnapshot(t *testing.T) {
	snap := deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-bj2-03", AzGroup: 3001}},
	})
	wfCtx := NewContext(map[string]any{"ZoneRegionIds": map[string]uint32{"cn-bj2-03": 999}})
	wfCtx.ReferenceData.ZoneCatalog = snap
	assert.Equal(t, uint32(3001), netOptimizerAzGroup(wfCtx, "cn-bj2-03"), "az_group from the catalog record")

	// Without a snapshot, the legacy map still answers.
	wfCtx2 := NewContext(map[string]any{"ZoneRegionIds": map[string]uint32{"cn-bj2-03": 999}})
	assert.Equal(t, uint32(999), netOptimizerAzGroup(wfCtx2, "cn-bj2-03"))
}
