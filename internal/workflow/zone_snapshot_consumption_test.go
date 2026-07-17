package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/deployment"
)

// TestWorkflowZonePlacement_PrefersSnapshotOverLegacyMaps proves the migration:
// when the run carries a zone catalog, the placement comes from that single
// record — NOT from the legacy per-zone maps, which here deliberately disagree.
func TestWorkflowZonePlacement_PrefersSnapshotOverLegacyMaps(t *testing.T) {
	snap := deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-bj2-03", Region: "cn-bj2", ZoneID: 6003, AzGroup: 3003, IsPod: true}},
	})
	wfCtx := NewContext(map[string]any{
		"ZoneIds":       map[string]uint32{"cn-bj2-03": 111},
		"ZoneRegionIds": map[string]uint32{"cn-bj2-03": 222},
		"ZoneIsPods":    map[string]bool{"cn-bj2-03": false},
	})
	wfCtx.referenceData.ZoneCatalog = snap

	got, err := workflowZonePlacement(wfCtx, "cn-bj2-03")

	require.NoError(t, err)
	assert.Equal(t, deployment.ZonePlacement{Zone: "cn-bj2-03", Region: "cn-bj2", ZoneID: 6003, AzGroup: 3003, IsPod: true}, got,
		"the single catalog record wins over the legacy maps")
}

// TestWorkflowZonePlacement_FallsBackOnlyWhenNoSnapshot pins the bridge: a run
// with NO snapshot at all (an unmigrated direct workflow-engine test) still
// resolves from the maps.
func TestWorkflowZonePlacement_FallsBackOnlyWhenNoSnapshot(t *testing.T) {
	wfCtx := NewContext(map[string]any{
		"ZoneIds":       map[string]uint32{"cn-sh2-02": 2002},
		"ZoneRegionIds": map[string]uint32{"cn-sh2-02": 3002},
		"ZoneIsPods":    map[string]bool{"cn-sh2-02": false},
	})

	got, err := workflowZonePlacement(wfCtx, "cn-sh2-02")

	require.NoError(t, err)
	assert.Equal(t, uint32(2002), got.ZoneID, "without any snapshot the legacy maps still resolve")
	assert.Equal(t, uint32(3002), got.AzGroup)
}

// TestWorkflowZonePlacement_PresentSnapshotNeverFallsBackToMaps is the fix for the
// review finding: a snapshot that is present but cannot answer must FAIL, not
// read a stale map. Both an unavailable snapshot and an available one missing the
// zone must error even though the legacy maps carry a (wrong) answer — otherwise a
// zone the authority rejected re-enters through the map, or a create proceeds on a
// zero placement.
func TestWorkflowZonePlacement_PresentSnapshotNeverFallsBackToMaps(t *testing.T) {
	legacyMaps := map[string]any{
		"ZoneIds":       map[string]uint32{"cn-bj2-03": 999},
		"ZoneRegionIds": map[string]uint32{"cn-bj2-03": 888},
		"ZoneIsPods":    map[string]bool{"cn-bj2-03": true},
	}

	t.Run("unavailable snapshot fails, does not read the map", func(t *testing.T) {
		wfCtx := NewContext(legacyMaps)
		wfCtx.referenceData.ZoneCatalog = deployment.NewZoneCatalogSnapshot(false, nil)

		_, err := workflowZonePlacement(wfCtx, "cn-bj2-03")
		require.Error(t, err, "an unavailable catalog must refuse, not fall back to the map's 999")
	})

	t.Run("available snapshot missing the zone fails, does not read the map", func(t *testing.T) {
		wfCtx := NewContext(legacyMaps)
		wfCtx.referenceData.ZoneCatalog = deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
			{Placement: deployment.ZonePlacement{Zone: "cn-sh2-02", ZoneID: 2002}},
		})

		_, err := workflowZonePlacement(wfCtx, "cn-bj2-03")
		require.Error(t, err, "a zone the catalog does not carry must refuse, not fall back to the map's 999")
	})
}

// TestNetOptimizerAzGroup_SnapshotIsAuthoritative pins the same tightening for the
// net-optimizer az_group source.
func TestNetOptimizerAzGroup_SnapshotIsAuthoritative(t *testing.T) {
	present := deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-bj2-03", AzGroup: 3001}},
	})

	// Snapshot present and resolving → its az_group, over a disagreeing map.
	wfCtx := NewContext(map[string]any{"ZoneRegionIds": map[string]uint32{"cn-bj2-03": 999}})
	wfCtx.referenceData.ZoneCatalog = present
	az, err := netOptimizerAzGroup(wfCtx, "cn-bj2-03")
	require.NoError(t, err)
	assert.Equal(t, uint32(3001), az)

	// No snapshot → legacy map answers.
	noSnap := NewContext(map[string]any{"ZoneRegionIds": map[string]uint32{"cn-bj2-03": 999}})
	az2, err := netOptimizerAzGroup(noSnap, "cn-bj2-03")
	require.NoError(t, err)
	assert.Equal(t, uint32(999), az2)

	// Present but unavailable → error, never the map's 999.
	down := NewContext(map[string]any{"ZoneRegionIds": map[string]uint32{"cn-bj2-03": 999}})
	down.referenceData.ZoneCatalog = deployment.NewZoneCatalogSnapshot(false, nil)
	_, err = netOptimizerAzGroup(down, "cn-bj2-03")
	require.Error(t, err)

	// Present but missing the zone → error, never the map's 999.
	missing := NewContext(map[string]any{"ZoneRegionIds": map[string]uint32{"cn-bj2-03": 999}})
	missing.referenceData.ZoneCatalog = present
	_, err = netOptimizerAzGroup(missing, "cn-nope-99")
	require.Error(t, err)
}
