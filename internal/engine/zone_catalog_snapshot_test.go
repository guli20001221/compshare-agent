package engine

import (
	"errors"
	"testing"
)

// TestZoneCatalogSnapshot_BuildsOneRecordPerZone pins that every placement field
// AND the display name of a zone come from the SAME upstream row — the single
// source of truth that replaces the four parallel maps (gate #5, #7).
func TestZoneCatalogSnapshot_BuildsOneRecordPerZone(t *testing.T) {
	eng := newZoneEngine(zoneCatalogExec(), "SHOULD-NOT-BE-USED")

	snap := eng.zoneCatalogSnapshot(zoneUserCtx())

	if !snap.Available() {
		t.Fatal("a live 3-zone catalog must be available")
	}
	p, ok := snap.Placement("cn-bj2-03")
	if !ok {
		t.Fatal("cn-bj2-03 must resolve from the live catalog")
	}
	if p.ZoneID != 5001 || p.AzGroup != 3003 || p.Region != "cn-bj2" || !p.IsPod {
		t.Errorf("placement fields must all come from the catalog row, got %+v", p)
	}
	if got := snap.Label("cn-bj2-03"); got != "华北一C" {
		t.Errorf("display name comes from the same row, got %q", got)
	}
	// A normal (non-pod) zone's flags also come from its own row.
	if wlcb, _ := snap.Placement("cn-wlcb-01"); wlcb.IsPod || wlcb.ZoneID != 10027 {
		t.Errorf("cn-wlcb-01 must be a non-pod zone with its own id, got %+v", wlcb)
	}
	// Order follows the catalog, so it is a stable form-option order.
	if zs := snap.Zones(); len(zs) != 3 || zs[0] != "cn-wlcb-01" {
		t.Errorf("zones must follow catalog order, got %v", zs)
	}
}

// TestZoneCatalogSnapshot_UnavailableWhenCatalogDown pins gate #2 at the engine
// boundary: a failed support-zone query yields an UNAVAILABLE snapshot, never a
// fallback table — the consumer must refuse rather than guess.
func TestZoneCatalogSnapshot_UnavailableWhenCatalogDown(t *testing.T) {
	exec := &mockExecutorFn{fn: func(string, map[string]any) (map[string]any, error) {
		return nil, errors.New("support-zone API down")
	}}
	eng := newZoneEngine(exec, "SHOULD-NOT-BE-USED")

	snap := eng.zoneCatalogSnapshot(zoneUserCtx())

	if snap.Available() {
		t.Error("a failed support-zone query must yield an unavailable snapshot, not a fallback")
	}
	if _, ok := snap.Placement("cn-bj2-03"); ok {
		t.Error("an unavailable snapshot must resolve nothing")
	}
}

// TestZoneCatalogSnapshotForAction_OnlyZoneWorkflowsFetch pins that a workflow
// with no zone (e.g. StopInstanceWorkflow) attaches no catalog and pays no read,
// while the three zone-sensitive creates do.
func TestZoneCatalogSnapshotForAction_OnlyZoneWorkflowsFetch(t *testing.T) {
	eng := newZoneEngine(zoneCatalogExec(), "SHOULD-NOT-BE-USED")
	ctx := zoneUserCtx()

	for _, action := range []string{"CreateInstanceWorkflow", "CreateCFSWorkflow", "EnableNetOptimizerWorkflow"} {
		if eng.zoneCatalogSnapshotForAction(ctx, action) == nil {
			t.Errorf("%s must receive a zone catalog", action)
		}
	}
	if eng.zoneCatalogSnapshotForAction(ctx, "StopInstanceWorkflow") != nil {
		t.Error("a non-zone workflow must not fetch a zone catalog")
	}
}
