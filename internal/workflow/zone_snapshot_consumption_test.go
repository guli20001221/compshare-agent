package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/deployment"
)

// TestWorkflowZonePlacement_ResolvesFromSnapshot proves a zone resolves to the
// single catalog record — ZoneID/Region/AzGroup/IsPod all from one row.
func TestWorkflowZonePlacement_ResolvesFromSnapshot(t *testing.T) {
	snap := deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-bj2-03", Region: "cn-bj2", ZoneID: 6003, AzGroup: 3003, IsPod: true}},
	})
	wfCtx := NewContext(map[string]any{})
	wfCtx.referenceData.ZoneCatalog = snap

	got, err := workflowZonePlacement(wfCtx, "cn-bj2-03")

	require.NoError(t, err)
	assert.Equal(t, deployment.ZonePlacement{Zone: "cn-bj2-03", Region: "cn-bj2", ZoneID: 6003, AzGroup: 3003, IsPod: true}, got,
		"the placement is the single catalog record")
}

// TestWorkflowZonePlacement_RequiresAvailableSnapshot pins the post-convergence
// contract: the snapshot is the sole authority, so a missing (nil), unavailable,
// or zone-absent snapshot is a hard failure — there is no per-zone map to fall
// back to, and Available() is nil-safe so a nil snapshot simply refuses.
func TestWorkflowZonePlacement_RequiresAvailableSnapshot(t *testing.T) {
	t.Run("nil snapshot refuses", func(t *testing.T) {
		_, err := workflowZonePlacement(NewContext(map[string]any{}), "cn-bj2-03")
		require.Error(t, err, "no snapshot attached must refuse, not guess")
	})

	t.Run("unavailable snapshot refuses", func(t *testing.T) {
		wfCtx := NewContext(map[string]any{})
		wfCtx.referenceData.ZoneCatalog = deployment.NewZoneCatalogSnapshot(false, nil)
		_, err := workflowZonePlacement(wfCtx, "cn-bj2-03")
		require.Error(t, err, "an unavailable catalog must refuse")
	})

	t.Run("available snapshot missing the zone refuses", func(t *testing.T) {
		wfCtx := NewContext(map[string]any{})
		wfCtx.referenceData.ZoneCatalog = deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
			{Placement: deployment.ZonePlacement{Zone: "cn-sh2-02", ZoneID: 2002}},
		})
		_, err := workflowZonePlacement(wfCtx, "cn-bj2-03")
		require.Error(t, err, "a zone the catalog does not carry must refuse")
	})
}

// TestResolveCreateCFSZone_ResolvesPodZoneFromSnapshot pins that CFS resolves its
// Pod-zone placement from the turn snapshot (no second support-zone query) and
// keeps its own Pod-only guard on the record the snapshot returns.
func TestResolveCreateCFSZone_ResolvesPodZoneFromSnapshot(t *testing.T) {
	snap := deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-pod-01", Region: "cn-pod", ZoneID: 7001, AzGroup: 3007, IsPod: true}},
		{Placement: deployment.ZonePlacement{Zone: "cn-sh2-02", ZoneID: 2002, AzGroup: 3002, IsPod: false}},
	})
	wfCtx := NewContext(map[string]any{})
	wfCtx.referenceData.ZoneCatalog = snap

	isPod, zoneID, azGroup, region, zone, err := resolveCreateCFSZone(wfCtx, "cn-pod-01")
	require.NoError(t, err)
	assert.True(t, isPod)
	assert.Equal(t, uint32(7001), zoneID)
	assert.Equal(t, uint32(3007), azGroup)
	assert.Equal(t, "cn-pod", region, "Region from the catalog record")
	assert.Equal(t, "cn-pod-01", zone)

	// CFS requires a Pod zone; a non-pod zone from the snapshot is refused.
	_, _, _, _, _, err = resolveCreateCFSZone(wfCtx, "cn-sh2-02")
	require.Error(t, err)
}

// TestZoneDisplayLabel_SnapshotLabelOrBareId pins that a form label comes from the
// snapshot record and degrades to the bare zone id on any resolution failure — a
// label is display-only and must never error.
func TestZoneDisplayLabel_SnapshotLabelOrBareId(t *testing.T) {
	snap := deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-bj2-03"}, DisplayName: "华北一C"},
	})
	present := NewContext(map[string]any{})
	present.referenceData.ZoneCatalog = snap
	assert.Equal(t, "华北一C", zoneDisplayLabel(present, "cn-bj2-03"), "label from the snapshot record")

	down := NewContext(nil)
	down.referenceData.ZoneCatalog = deployment.NewZoneCatalogSnapshot(false, nil)
	assert.Equal(t, "cn-bj2-03", zoneDisplayLabel(down, "cn-bj2-03"), "unresolvable → bare id, never an error")
}

// TestWorkflowZoneIDIndex_FromSnapshot pins the inventory id→zone index is built
// from the snapshot; an absent snapshot yields an empty index (nil-safe).
func TestWorkflowZoneIDIndex_FromSnapshot(t *testing.T) {
	snap := deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-bj2-03", ZoneID: 6003}},
		{Placement: deployment.ZonePlacement{Zone: "cn-sh2-02", ZoneID: 2002}},
	})
	present := NewContext(map[string]any{})
	present.referenceData.ZoneCatalog = snap
	idx := workflowZoneIDIndex(present)
	assert.Equal(t, "cn-bj2-03", idx[6003])
	assert.Equal(t, "cn-sh2-02", idx[2002])

	assert.Empty(t, workflowZoneIDIndex(NewContext(map[string]any{})), "no snapshot → empty index, no map fallback")
}

// TestNetOptimizerNormalize_RegionAndAzGroupFromOneRecord pins that the
// net-optimizer takes BOTH Region and az_group from a single placement record,
// without deriving Region from the zone string.
func TestNetOptimizerNormalize_RegionAndAzGroupFromOneRecord(t *testing.T) {
	snap := deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-bj2-03", Region: "cn-bj2", AzGroup: 3001}},
	})
	wfCtx := NewContext(map[string]any{"Zone": "cn-bj2-03"})
	wfCtx.referenceData.ZoneCatalog = snap

	require.NoError(t, normalizeNetOptimizerParams(wfCtx))
	assert.Equal(t, "cn-bj2", wfCtx.Params["Region"], "Region from the catalog record")
	assert.Equal(t, uint32(3001), wfCtx.Params["NetOptimizerAzGroup"], "az_group from the same record")
}

// TestNetOptimizerNormalize_SnapshotRegionNotOverriddenByParam pins that a
// contradictory Region param cannot override the record's Region — otherwise the
// "single source" is only half true (az_group from the record, Region from the param).
func TestNetOptimizerNormalize_SnapshotRegionNotOverriddenByParam(t *testing.T) {
	snap := deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-bj2-03", Region: "cn-bj2", AzGroup: 3001}},
	})
	wfCtx := NewContext(map[string]any{"Zone": "cn-bj2-03", "Region": "cn-wlcb"}) // contradictory param
	wfCtx.referenceData.ZoneCatalog = snap

	require.NoError(t, normalizeNetOptimizerParams(wfCtx))
	assert.Equal(t, "cn-bj2", wfCtx.Params["Region"], "the catalog record's Region wins; a param cannot override the snapshot")
}

// TestNetOptimizerNormalize_RequiresAvailableSnapshot pins that the net-optimizer
// refuses on a missing (nil), zone-absent, or unavailable snapshot — no map fallback.
func TestNetOptimizerNormalize_RequiresAvailableSnapshot(t *testing.T) {
	present := deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-sh2-02", Region: "cn-sh2", AzGroup: 3002}},
	})

	nilSnap := NewContext(map[string]any{"Zone": "cn-bj2-03"})
	assert.Error(t, normalizeNetOptimizerParams(nilSnap), "no snapshot → refuse")

	missing := NewContext(map[string]any{"Zone": "cn-bj2-03"})
	missing.referenceData.ZoneCatalog = present
	assert.Error(t, normalizeNetOptimizerParams(missing), "zone absent from the catalog → refuse")

	down := NewContext(map[string]any{"Zone": "cn-bj2-03"})
	down.referenceData.ZoneCatalog = deployment.NewZoneCatalogSnapshot(false, nil)
	assert.Error(t, normalizeNetOptimizerParams(down), "unavailable catalog → refuse")
}

// TestAddZoneRegionAndID_RegionAndIDFromOneRecord pins that the read-probe stamps
// Region AND zone_id from a SINGLE catalog record, never a snapshot id paired with
// a zone-string-guessed Region. The record's Region is deliberately unequal to
// a string-derived region so a guess is distinguishable; a zone the catalog rejects
// gets neither field.
func TestAddZoneRegionAndID_RegionAndIDFromOneRecord(t *testing.T) {
	snap := deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-bj2-03", Region: "cn-realbj", ZoneID: 6003}},
	})

	t.Run("both fields from the snapshot record", func(t *testing.T) {
		wfCtx := NewContext(map[string]any{})
		wfCtx.referenceData.ZoneCatalog = snap
		args := addZoneRegionAndID(wfCtx, map[string]any{}, "cn-bj2-03")
		assert.Equal(t, "cn-realbj", args["Region"], "Region from the catalog record, not regionFromZone's cn-bj2")
		assert.Equal(t, uint32(6003), args["zone_id"], "zone_id from the same record")
	})

	t.Run("present-but-missing snapshot stamps nothing, no string-guess Region", func(t *testing.T) {
		wfCtx := NewContext(map[string]any{})
		wfCtx.referenceData.ZoneCatalog = snap // does not carry cn-sh2-02
		args := addZoneRegionAndID(wfCtx, map[string]any{}, "cn-sh2-02")
		_, hasRegion := args["Region"]
		_, hasID := args["zone_id"]
		assert.False(t, hasRegion, "a zone the catalog rejects must not get a string-guessed Region")
		assert.False(t, hasID, "nor a zone_id")
	})
}

// TestNormalizeCreateCFSParams_RegionSingleSourceFromSnapshot pins that the CFS
// Region comes ONLY from the catalog record — a contradictory Region param cannot
// override it, and a record missing Region fails closed instead of being back-filled
// by trimming the zone string. The pod zone id is chosen so that guess differs from the
// record Region, making a guess visible.
func TestNormalizeCreateCFSParams_RegionSingleSourceFromSnapshot(t *testing.T) {
	pod := deployment.ZonePlacement{Zone: "cn-pod-bj-01", Region: "cn-realpod", ZoneID: 7001, AzGroup: 3007, IsPod: true}

	t.Run("record Region wins over a contradictory param and over a zone-string guess", func(t *testing.T) {
		snap := deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{{Placement: pod}})
		wfCtx := NewContext(map[string]any{
			"Name": "cfs1", "Size": float64(100), "Zone": "cn-pod-bj-01",
			"Region": "cn-wrong", "ChargeType": "Month",
		})
		wfCtx.referenceData.ZoneCatalog = snap
		require.NoError(t, normalizeCreateCFSParams(wfCtx))
		assert.Equal(t, "cn-realpod", wfCtx.Params["Region"],
			"Region from the catalog record, not the param cn-wrong nor the guess cn-pod-bj")
	})

	t.Run("record missing Region fails closed, no string-derived fallback", func(t *testing.T) {
		noRegion := deployment.ZonePlacement{Zone: "cn-pod-bj-01", ZoneID: 7001, AzGroup: 3007, IsPod: true}
		snap := deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{{Placement: noRegion}})
		wfCtx := NewContext(map[string]any{
			"Name": "cfs1", "Size": float64(100), "Zone": "cn-pod-bj-01", "ChargeType": "Month",
		})
		wfCtx.referenceData.ZoneCatalog = snap
		require.Error(t, normalizeCreateCFSParams(wfCtx),
			"a catalog record with no Region must refuse, not derive one from the zone string")
	})
}

// TestZoneFormOptions_LabelsFromSnapshot pins the confirm-card zone selector labels
// each option from the snapshot record, on BOTH the current-zone head and the loop path.
func TestZoneFormOptions_LabelsFromSnapshot(t *testing.T) {
	snap := deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-bj2-03"}, DisplayName: "华北一C"},
		{Placement: deployment.ZonePlacement{Zone: "cn-sh2-02"}, DisplayName: "华东二B"},
	})
	wfCtx := NewContext(map[string]any{})
	wfCtx.referenceData.ZoneCatalog = snap
	catalog := map[string]any{"AvailableInstanceTypes": []any{
		map[string]any{"Name": "RTX4090", "Zone": "cn-bj2-03", "Status": "Normal"},
		map[string]any{"Name": "RTX4090", "Zone": "cn-sh2-02", "Status": "Normal"},
	}}

	opts := zoneFormOptions(wfCtx, catalog, "RTX4090", "cn-bj2-03")

	labels := map[string]string{}
	for _, o := range opts {
		labels[o.Value] = o.Label
	}
	assert.Equal(t, "华北一C", labels["cn-bj2-03"], "current-zone head label from the snapshot")
	assert.Equal(t, "华东二B", labels["cn-sh2-02"], "loop-path label from the snapshot")
}
