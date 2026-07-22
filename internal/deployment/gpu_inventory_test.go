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
