package workflow

import (
	"testing"

	"github.com/compshare-agent/internal/deployment"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func guidedInventoryZoneCatalog() *deployment.ZoneCatalogSnapshot {
	return deployment.NewZoneCatalogSnapshot(true, []deployment.ZoneCatalogEntry{
		{Placement: deployment.ZonePlacement{Zone: "cn-wlcb-01", ZoneID: 1}, DisplayName: "华北二A"},
		{Placement: deployment.ZonePlacement{Zone: "cn-bj2-03", ZoneID: 5001, AzGroup: 11, IsPod: true}, DisplayName: "华北一C"},
		{Placement: deployment.ZonePlacement{Zone: "cn-wlcb-03", ZoneID: 10033, AzGroup: 12, IsPod: true}, DisplayName: "华北二C"},
	})
}

func TestGuidedCreateGPUInventoryQueriesBothBackendsAndMergesByZoneAuthority(t *testing.T) {
	wfCtx := NewContext(map[string]any{"GpuType": "4090"})
	wfCtx.referenceData = ReferenceData{ZoneCatalog: guidedInventoryZoneCatalog()}

	officialArgs, err := stepQueryOfficialGPUInventory().BuildArgs(wfCtx)
	require.NoError(t, err)
	assert.NotContains(t, officialArgs, "zone_id")
	podArgs, err := stepQueryPodGPUInventory().BuildArgs(wfCtx)
	require.NoError(t, err)
	assert.NotZero(t, podArgs["zone_id"])

	wfCtx.StepResults[createOfficialGPUInventoryStep] = map[string]any{"GpuInventory": map[string]any{
		"Exclusive": map[string]any{
			"1":     map[string]any{"4090": float64(20)},
			"5001":  map[string]any{"4090": float64(0)},
			"10033": map[string]any{"4090": float64(0)},
		},
		"Spot": map[string]any{},
	}}
	wfCtx.StepResults[createPodGPUInventoryStep] = map[string]any{"GpuInventory": map[string]any{
		"Exclusive": map[string]any{"5001": map[string]any{"4090": float64(12)}},
		"Spot":      map[string]any{"10033": map[string]any{"4090": float64(7)}},
	}}

	merged, err := stepResolveGPUInventorySnapshot().Resolve(wfCtx)
	require.NoError(t, err)
	inv := merged["GpuInventory"].(map[string]any)
	exclusive := inv[deployment.GPUInventoryPoolExclusive].(map[string]any)
	spot := inv[deployment.GPUInventoryPoolSpot].(map[string]any)
	assert.Equal(t, uint32(12), exclusive["5001"].(map[string]any)["4090"])
	assert.Equal(t, uint32(7), spot["10033"].(map[string]any)["4090"])
	assert.Equal(t, uint32(20), exclusive["1"].(map[string]any)["4090"])
}

func guidedInventoryContext(t *testing.T, zone, chargeType string) *Context {
	t.Helper()
	wfCtx := NewContext(map[string]any{"GpuType": "4090", "Zone": zone, "ChargeType": chargeType})
	wfCtx.referenceData = ReferenceData{ZoneCatalog: guidedInventoryZoneCatalog()}
	wfCtx.StepResults[createOfficialGPUInventoryStep] = map[string]any{"GpuInventory": map[string]any{
		"Exclusive": map[string]any{"1": map[string]any{"4090": float64(0)}},
	}}
	wfCtx.StepResults[createPodGPUInventoryStep] = map[string]any{"GpuInventory": map[string]any{
		"Exclusive": map[string]any{"5001": map[string]any{"4090": float64(0)}},
		"Spot":      map[string]any{"10033": map[string]any{"4090": float64(0)}},
	}}
	merged, err := stepResolveGPUInventorySnapshot().Resolve(wfCtx)
	require.NoError(t, err)
	wfCtx.StepResults[createGPUInventoryStep] = merged
	return wfCtx
}

func TestGuidedCreateChargeOptionsUseProductPoolSupportNotPodGuess(t *testing.T) {
	tests := []struct {
		name, zone string
		enabled    []string
		disabled   []string
	}{
		{"华北一C is exclusive", "cn-bj2-03", []string{"Postpay", "Day", "Month"}, []string{"Spot"}},
		{"华北二C is spot", "cn-wlcb-03", []string{"Spot"}, []string{"Postpay", "Day", "Month"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wfCtx := guidedInventoryContext(t, tt.zone, "Postpay")
			opts := createChargeTypeOptions(wfCtx, tt.zone)
			byValue := map[string]ConfirmFormOption{}
			for _, opt := range opts {
				byValue[opt.Value] = opt
			}
			for _, value := range tt.enabled {
				assert.False(t, byValue[value].Disabled, value)
			}
			for _, value := range tt.disabled {
				assert.True(t, byValue[value].Disabled, value)
				assert.Contains(t, byValue[value].Reason, "不支持")
			}
		})
	}
}

func TestGuidedCreatePlacementRejectsOnlyUnsupportedPurchasePool(t *testing.T) {
	spot := guidedInventoryContext(t, "cn-wlcb-03", deployment.ChargeTypeSpot)
	placement, ok := spot.ZoneCatalog().Placement("cn-wlcb-03")
	require.True(t, ok)
	require.NoError(t, validateCreatePlacement(spot, placement, true), "华北二C must accept its real Spot pool")

	spot.Params["ChargeType"] = deployment.ChargeTypePostpay
	err := validateCreatePlacement(spot, placement, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "不支持独占购买方式")

	exclusive := guidedInventoryContext(t, "cn-bj2-03", deployment.ChargeTypeSpot)
	placement, ok = exclusive.ZoneCatalog().Placement("cn-bj2-03")
	require.True(t, ok)
	err = validateCreatePlacement(exclusive, placement, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "不支持抢占式购买方式")
}
