package knowledge

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// availType builds one AvailableInstanceTypes entry in the nested {Rate,Value}
// shape the real DescribeAvailableCompShareInstanceTypes response uses.
func availType(name, status string, vram, perf, maxGPU int) map[string]any {
	return map[string]any{
		"Name":           name,
		"Status":         status,
		"GraphicsMemory": map[string]any{"Value": float64(vram), "Rate": float64(0)},
		"Performance":    map[string]any{"Value": float64(perf), "Rate": float64(0)},
		"MachineSizes":   []any{map[string]any{"Gpu": float64(1)}, map[string]any{"Gpu": float64(maxGPU)}},
	}
}

func TestParseAvailableGPUs(t *testing.T) {
	result := map[string]any{"AvailableInstanceTypes": []any{
		availType("4090", "Normal", 24, 83, 8),
		availType("A100", "Normal", 80, 100, 8),
		availType("OldCard", "SoldOut", 16, 10, 8),                                                             // excluded: sold out
		map[string]any{"Name": "NoVRAM", "Status": "Normal"},                                                   // excluded: no VRAM
		map[string]any{"Name": "", "Status": "Normal", "GraphicsMemory": map[string]any{"Value": float64(24)}}, // excluded: no name
	}}
	got := ParseAvailableGPUs(result, "")
	require.Len(t, got, 2, "sold-out / no-VRAM / no-name entries are skipped")
	byName := map[string]AvailableGPU{}
	for _, g := range got {
		byName[g.Name] = g
	}
	assert.Equal(t, 24, byName["4090"].VRAMGB)
	assert.Equal(t, float64(83), byName["4090"].Perf)
	assert.Equal(t, 8, byName["4090"].MaxGPU, "max Gpu across MachineSizes")
	assert.Equal(t, 80, byName["A100"].VRAMGB)
	assert.Nil(t, ParseAvailableGPUs(nil, "cn-wlcb-01"))
}

// TestParseAvailableGPUs_ZoneFilter mirrors the real 2026-05-31 topology: region
// cn-wlcb returns both cn-wlcb-01 AND the Shanghai cn-sh2-02 zone. Filtering to the
// create-zone must drop cn-sh2-02-only cards so they can't be recommended for a
// cn-wlcb-01 create; zone=="" keeps everything.
func TestParseAvailableGPUs_ZoneFilter(t *testing.T) {
	withZone := func(z string, m map[string]any) map[string]any { m["Zone"] = z; return m }
	result := map[string]any{"AvailableInstanceTypes": []any{
		withZone("cn-wlcb-01", availType("4090", "Normal", 24, 83, 8)),
		withZone("cn-sh2-02", availType("2080Ti", "Normal", 11, 13, 8)), // sh2-only
		withZone("cn-wlcb-01", availType("A100", "Normal", 80, 100, 8)),
	}}
	names := func(gs []AvailableGPU) map[string]bool {
		s := map[string]bool{}
		for _, g := range gs {
			s[g.Name] = true
		}
		return s
	}
	wlcb := names(ParseAvailableGPUs(result, "cn-wlcb-01"))
	assert.Equal(t, map[string]bool{"4090": true, "A100": true}, wlcb, "only cn-wlcb-01 cards; cn-sh2-02-only 2080Ti dropped")
	assert.True(t, names(ParseAvailableGPUs(result, ""))["2080Ti"], "empty zone keeps all zones")
}
