package workflow

import (
	"context"

	"github.com/compshare-agent/internal/deployment"
)

// runCreateTest runs a create-family workflow with the common create-zone catalog
// attached as reference data — post-convergence the workflow resolves every zone
// from that snapshot. A test that needs a tailored zone set passes its own With*
// option; applied last, it overrides the default (WithReferenceData is last-wins).
func (e *Engine) runCreateTest(def *Definition, params map[string]any, opts ...RunOption) (*Result, error) {
	return e.Run(context.Background(), def, params, append([]RunOption{withCreateZones()}, opts...)...)
}

// createZoneCatalog is the common zone catalog most create tests need: the default
// create zone cn-wlcb-01 (non-pod) plus a Pod zone and a third region, matching the
// zones the create read-mocks surface. Post-convergence the workflow resolves every
// zone from this snapshot (attached as reference data), never a per-zone param map.
func createZoneCatalog() *deployment.ZoneCatalogSnapshot {
	return deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-wlcb-01", Region: "cn-wlcb", ZoneID: 10027, IsPod: false}, DisplayName: "华北二A"},
		{Placement: deployment.ZonePlacement{Zone: "cn-bj2-03", Region: "cn-bj2", ZoneID: 5001, AzGroup: 3003, IsPod: true}, DisplayName: "华北一C"},
		{Placement: deployment.ZonePlacement{Zone: "cn-sh2-02", Region: "cn-sh2", ZoneID: 8200, AzGroup: 3002, IsPod: false}, DisplayName: "上海二B"},
	})
}

// withCreateZones attaches the common create-zone catalog as the run's reference data.
func withCreateZones() RunOption {
	return WithReferenceData(ReferenceData{ZoneCatalog: createZoneCatalog()})
}

// withZoneCatalog attaches a specific snapshot, for tests that need a tailored zone set.
func withZoneCatalog(snap *deployment.ZoneCatalogSnapshot) RunOption {
	return WithReferenceData(ReferenceData{ZoneCatalog: snap})
}

// podZoneCatalog is a one-record Pod-zone snapshot, for Context-builder tests that
// set referenceData directly (not via a Run option).
func podZoneCatalog(zone, region string, zoneID, azGroup uint32) *deployment.ZoneCatalogSnapshot {
	return deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: zone, Region: region, ZoneID: zoneID, AzGroup: azGroup, IsPod: true}},
	})
}

// withPodZone attaches a single Pod-zone record — for create tests that route a
// specific Pod zone through the capacity/placement path.
func withPodZone(zone, region string, zoneID, azGroup uint32) RunOption {
	return withZoneCatalog(podZoneCatalog(zone, region, zoneID, azGroup))
}

// withNormalZone attaches a single non-Pod zone record — for create tests that
// route a specific normal zone (Zone/Region carried through capacity).
func withNormalZone(zone, region string, zoneID uint32) RunOption {
	return withZoneCatalog(deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: zone, Region: region, ZoneID: zoneID, IsPod: false}},
	}))
}

// inventoryZoneCatalog matches the guided GPU-inventory fixtures: cn-wlcb-01 and
// cn-sh2-02 carry the small numeric zone ids (1, 2) the inventory is keyed by, so
// workflowZoneIDIndex maps each inventory bucket back to its zone.
func inventoryZoneCatalog() *deployment.ZoneCatalogSnapshot {
	return deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-wlcb-01", Region: "cn-wlcb", ZoneID: 1}, DisplayName: "华北二A"},
		{Placement: deployment.ZonePlacement{Zone: "cn-sh2-02", Region: "cn-sh2", ZoneID: 2}, DisplayName: "上海二B"},
	})
}

// noDescribeZoneCatalog is a snapshot carrying the given zones with NO display name
// — the manual/CLI path where the catalog has no describe, so form labels fall back
// to the bare zone id.
func noDescribeZoneCatalog(zones ...string) *deployment.ZoneCatalogSnapshot {
	entries := make([]deployment.ZoneCatalogEntry, 0, len(zones))
	for _, z := range zones {
		entries = append(entries, deployment.ZoneCatalogEntry{Placement: deployment.ZonePlacement{Zone: z}})
	}
	return deployment.NewZoneCatalogSnapshot(true, entries)
}
