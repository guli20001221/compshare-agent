package deployment

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func inventoryTestZoneCatalog() *ZoneCatalogSnapshot {
	return NewZoneCatalogSnapshot(true, []ZoneCatalogEntry{
		{Placement: ZonePlacement{Zone: "cn-wlcb-01", ZoneID: 10027}, DisplayName: "华北二A"},
		{Placement: ZonePlacement{Zone: "cn-bj2-03", ZoneID: 5001, IsPod: true}, DisplayName: "华北一C"},
		{Placement: ZonePlacement{Zone: "cn-wlcb-03", ZoneID: 10033, IsPod: true}, DisplayName: "华北二C"},
	})
}

func TestGPUInventorySnapshotKeepsEachZoneOnItsAuthoritativeBackend(t *testing.T) {
	catalog := inventoryTestZoneCatalog()
	official := map[string]any{
		"GpuInventory": map[string]any{
			"Exclusive": map[string]any{
				"10027": map[string]any{"4090": float64(17)},
				// These are the misleading zeros the official simulator can emit
				// for zones that actually belong to the Pod backend.
				"5001":  map[string]any{"4090": float64(0)},
				"10033": map[string]any{"4090": float64(0)},
			},
		},
		"UpdateTime": float64(101),
	}
	pod := map[string]any{
		"GpuInventory": map[string]any{
			"Exclusive": map[string]any{
				"5001": map[string]any{"4090": float64(9)},
			},
			"Spot": map[string]any{
				"10033": map[string]any{"4090": float64(6)},
			},
		},
		"UpdateTime": float64(202),
	}

	snapshot := NewGPUInventorySnapshot(catalog, official, true, true, pod, true, true)

	exclusive, spot, present, available := snapshot.Counts("cn-wlcb-01", "4090")
	require.True(t, available)
	require.True(t, present)
	assert.Equal(t, uint32(17), exclusive)
	assert.Zero(t, spot)

	exclusive, spot, present, available = snapshot.Counts("cn-bj2-03", "4090")
	require.True(t, available)
	require.True(t, present)
	assert.Equal(t, uint32(9), exclusive, "the official-pool zero must not shadow the Pod count")
	assert.Zero(t, spot)

	exclusive, spot, present, available = snapshot.Counts("cn-wlcb-03", "4090")
	require.True(t, available)
	require.True(t, present)
	assert.Zero(t, exclusive)
	assert.Equal(t, uint32(6), spot, "华北二C is a Pod Spot zone")

	assert.Equal(t, int64(101), snapshot.SourceState(GPUInventorySourceOfficial).UpdateTime)
	assert.Equal(t, int64(202), snapshot.SourceState(GPUInventorySourcePod).UpdateTime)
	zoneID, ok := PodSelectorZoneID(catalog)
	assert.True(t, ok)
	assert.Contains(t, []uint32{5001, 10033}, zoneID)
}

func TestGPUInventorySnapshotDoesNotTurnUnavailableSourceIntoZero(t *testing.T) {
	snapshot := NewGPUInventorySnapshot(inventoryTestZoneCatalog(), nil, true, false, nil, true, false)

	_, _, present, available := snapshot.Counts("cn-bj2-03", "4090")
	assert.False(t, available)
	assert.False(t, present)
	assert.Equal(t, map[string]any{
		"Attempted": true, "Available": false, "UpdateTime": int64(0),
	}, snapshot.ToResultMap()["InventorySources"].(map[string]any)[GPUInventorySourcePod])
}

func TestGPUInventorySnapshotPreservesSuccessfulExplicitZero(t *testing.T) {
	pod := map[string]any{"GpuInventory": map[string]any{
		"Exclusive": map[string]any{"5001": map[string]any{"4090": float64(0)}},
	}}
	snapshot := NewGPUInventorySnapshot(inventoryTestZoneCatalog(), nil, false, false, pod, true, true)

	exclusive, _, present, available := snapshot.Counts("cn-bj2-03", "4090")
	assert.True(t, available)
	assert.True(t, present)
	assert.Zero(t, exclusive)
}

func TestGPUInventorySnapshotSeparatesPoolSupportFromObservedCount(t *testing.T) {
	catalog := inventoryTestZoneCatalog()
	official := map[string]any{
		"GpuInventory": map[string]any{
			"Exclusive": map[string]any{"10027": map[string]any{"4090": float64(0), "A100": float64(0)}},
		},
		"SpotUnsupportedGpuTypes": []any{"A100"},
	}
	pod := map[string]any{"GpuInventory": map[string]any{
		"Exclusive": map[string]any{"5001": map[string]any{"4090": float64(0)}},
		"Spot":      map[string]any{"10033": map[string]any{"4090": float64(0)}},
	}}
	snapshot := NewGPUInventorySnapshot(catalog, official, true, true, pod, true, true)

	tests := []struct {
		zone, gpu, pool string
		supported       bool
		known           bool
	}{
		{"cn-bj2-03", "4090", GPUInventoryPoolExclusive, true, true},
		{"cn-bj2-03", "4090", GPUInventoryPoolSpot, false, true},
		{"cn-wlcb-03", "4090", GPUInventoryPoolExclusive, false, true},
		{"cn-wlcb-03", "4090", GPUInventoryPoolSpot, true, true},
		{"cn-wlcb-01", "4090", GPUInventoryPoolExclusive, true, true},
		{"cn-wlcb-01", "4090", GPUInventoryPoolSpot, true, true},
		{"cn-wlcb-01", "A100", GPUInventoryPoolSpot, false, true},
		{"cn-bj2-03", "missing", GPUInventoryPoolExclusive, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.zone+"/"+tt.gpu+"/"+tt.pool, func(t *testing.T) {
			supported, known := snapshot.PoolSupported(tt.zone, tt.gpu, tt.pool)
			assert.Equal(t, tt.supported, supported)
			assert.Equal(t, tt.known, known)
		})
	}

	result := snapshot.ToResultMap()
	placement, ok := catalog.Placement("cn-wlcb-03")
	require.True(t, ok)
	supported, known := InventoryPoolSupportFromResult(result, placement, "4090", GPUInventoryPoolSpot)
	assert.True(t, supported)
	assert.True(t, known, "the workflow result must preserve the same support fact")
}

func TestGPUInventorySnapshotUnavailableSourceLeavesPoolSupportUnknown(t *testing.T) {
	snapshot := NewGPUInventorySnapshot(inventoryTestZoneCatalog(), nil, true, false, nil, true, false)
	supported, known := snapshot.PoolSupported("cn-bj2-03", "4090", GPUInventoryPoolExclusive)
	assert.False(t, supported)
	assert.False(t, known)
}
