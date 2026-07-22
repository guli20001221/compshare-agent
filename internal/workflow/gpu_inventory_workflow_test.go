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
		{Placement: deployment.ZonePlacement{Zone: "cn-bj2-03", ZoneID: 5001, IsPod: true}, DisplayName: "华北一C"},
		{Placement: deployment.ZonePlacement{Zone: "cn-wlcb-03", ZoneID: 10033, IsPod: true}, DisplayName: "华北二C"},
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
