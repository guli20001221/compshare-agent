package deployment

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveBootDiskHonoursEveryMinimum(t *testing.T) {
	images := map[string]any{"ImageSet": []any{map[string]any{
		"CompShareImageId": "img-1", "Size": float64(50 * 1024),
	}}}
	catalog := map[string]any{"AvailableInstanceTypes": []any{map[string]any{
		"Name": "H20", "Zone": "cn-wlcb-01",
		"Disks": []any{map[string]any{"BootDisk": []any{map[string]any{
			"Name": "CLOUD_SSD", "MinimalSize": float64(100),
		}}}},
	}}}

	for name, requested := range map[string]uint32{"too small": 1, "larger request": 190} {
		t.Run(name, func(t *testing.T) {
			disks := ResolveBootDisk(images, catalog, "img-1", "H20", "cn-wlcb-01", requested)
			require.Len(t, disks, 1)
			disk := disks[0].(map[string]any)
			want := uint32(100)
			if requested > want {
				want = requested
			}
			require.Equal(t, want, disk["Size"])
		})
	}
}

func TestCatalogDataDiskRangeUsesTheExactGPUAndZone(t *testing.T) {
	catalog := map[string]any{"AvailableInstanceTypes": []any{
		map[string]any{"Name": "H20", "Zone": "cn-wlcb-01", "Disks": []any{map[string]any{
			"DataDisk": []any{map[string]any{"Name": "CLOUD_SSD", "MinimalSize": float64(10), "MaximalSize": float64(8000)}},
		}}},
		map[string]any{"Name": "H20", "Zone": "cn-sh2-02", "Disks": []any{map[string]any{
			"DataDisk": []any{map[string]any{"Name": "CLOUD_SSD", "MinimalSize": float64(20), "MaximalSize": float64(4000)}},
		}}},
	}}

	rangeSpec, ok := CatalogDataDiskRange(catalog, "H20", "cn-wlcb-01", DiskTypeCloudSSD)
	require.True(t, ok)
	require.Equal(t, uint32(10), rangeSpec.MinimumGB)
	require.Equal(t, uint32(8000), rangeSpec.MaximumGB)

	_, ok = CatalogDataDiskRange(catalog, "H20", "cn-bj2-03", DiskTypeCloudSSD)
	require.False(t, ok, "data-disk support must not fall back to another zone")
}
