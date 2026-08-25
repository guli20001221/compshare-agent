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
