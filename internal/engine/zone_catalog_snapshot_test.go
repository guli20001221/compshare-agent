package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/zones"
)

// zoneFailExecutor serves the create read steps but fails the support-zone query,
// so the write-path snapshot is unavailable while the draft resolution still
// reaches the zone lookup.
type zoneFailExecutor struct{ calls []string }

func (m *zoneFailExecutor) Execute(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	m.calls = append(m.calls, action)
	switch action {
	case "DescribeCompShareSupportZone":
		return nil, errors.New("support-zone API down")
	case "DescribeCompShareImages":
		return map[string]any{"ImageSet": []any{map[string]any{"CompShareImageId": "img-1", "Name": "PyTorch", "ImageType": "App"}}}, nil
	case "DescribeAvailableCompShareInstanceTypes":
		return map[string]any{"AvailableInstanceTypes": []any{availableGPU("4090", 24)}}, nil
	case "DescribeCompShareInstance":
		return map[string]any{"TotalCount": float64(0), "UHostSet": []any{}}, nil
	}
	return map[string]any{"RetCode": float64(0)}, nil
}

// TestExecuteWorkflow_ZoneCatalogFailureAbortsCreateBeforeConfirm makes permanent
// the end-to-end counter-example: when the zone catalog cannot be obtained, a
// create is refused BEFORE the confirmation gate and the real create API is never
// called. With strict reads a write never runs on an unavailable (or stale) zone
// catalog.
func TestExecuteWorkflow_ZoneCatalogFailureAbortsCreateBeforeConfirm(t *testing.T) {
	exec := &zoneFailExecutor{}
	confirmCalls := 0
	eng := NewWithDeps(&mockLLM{}, exec, func(string, map[string]any) bool { confirmCalls++; return true })
	eng.zoneCatalog = zones.NewCatalog(0) // fresh: the failing fetch is not masked by a shared cache

	reply := eng.executeResolvedWorkflow(zoneUserCtx(),
		mustConfirmable("CreateInstanceWorkflow", map[string]any{"GpuType": "4090", "ImageName": "PyTorch"}, zoneRefData(eng.zoneCatalogSnapshot(zoneUserCtx()))), noopStep)

	if confirmCalls != 0 {
		t.Errorf("the create must be refused before the confirmation gate, got %d confirm calls", confirmCalls)
	}
	for _, c := range exec.calls {
		if c == "CreateCompShareInstance" {
			t.Error("the real create API must never be called on an unavailable catalog")
		}
	}
	if !strings.Contains(reply, "可用区目录当前不可用") {
		t.Errorf("reply should carry the catalog-unavailable error, got: %s", reply)
	}
}

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
	if sh2, ok := snap.Entry("cn-sh2-02"); !ok || !sh2.DisableImageSync {
		t.Errorf("image-sync capability must come from the same live catalog row, got %+v ok=%v", sh2, ok)
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

// TestZoneCatalogSnapshot_SuccessfulEmptyCatalogIsAvailable pins the boundary S1
// defines but S4 blurred: a query that SUCCEEDS with no zones is an available,
// empty catalog — not a failure. Only "could not obtain it" is unavailable. The
// difference matters because an unavailable catalog forces a refuse, while an
// available-but-empty one is a real (if unhelpful) answer.
func TestZoneCatalogSnapshot_SuccessfulEmptyCatalogIsAvailable(t *testing.T) {
	exec := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		if action == "DescribeCompShareSupportZone" {
			return map[string]any{"ZoneInfo": []any{}}, nil // success, but zero zones
		}
		return map[string]any{"RetCode": float64(0)}, nil
	}}
	eng := newZoneEngine(exec, "SHOULD-NOT-BE-USED")

	snap := eng.zoneCatalogSnapshot(zoneUserCtx())

	if !snap.Available() {
		t.Error("a successful empty support-zone query must be an available (empty) catalog, not unavailable")
	}
	if _, ok := snap.Placement("cn-bj2-03"); ok {
		t.Error("an empty catalog resolves nothing")
	}
}

// One turn must not fetch two catalogs: a zone-catalog read followed by a write
// proposal must see the exact same immutable object. This test pins the engine
// choke point rather than relying on the process cache to make two calls happen
// to return equal data.
func TestZoneCatalogSnapshot_IsOneObjectAndOneFetchPerTurn(t *testing.T) {
	calls := 0
	exec := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		if action == "DescribeCompShareSupportZone" {
			calls++
			return zoneCatalogExec().Execute(context.Background(), action, nil)
		}
		return map[string]any{"RetCode": float64(0)}, nil
	}}
	eng := newZoneEngine(exec, "SHOULD-NOT-BE-USED")
	eng.currentCtx = zoneUserCtx() // marks an active turn; Chat normally owns this
	defer func() { eng.currentCtx = nil }()

	first := eng.zoneCatalogSnapshot(zoneUserCtx())
	second := eng.zoneCatalogSnapshot(zoneUserCtx())

	if first != second {
		t.Fatal("all zone consumers in one turn must receive the same snapshot object")
	}
	if calls != 1 {
		t.Fatalf("one turn must fetch the support-zone catalog once, got %d calls", calls)
	}
}
