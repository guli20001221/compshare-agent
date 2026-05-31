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

// TestRecommendGPUTypeLive pins the staleness fix: GPU sizing tracks the LIVE
// available-card set, so a sold-out card is excluded and a brand-new card the
// static gpuSpecs table has never heard of is still selectable; an empty live set
// degrades to the static-table path.
func TestRecommendGPUTypeLive(t *testing.T) {
	live := []AvailableGPU{
		{Name: "4090", VRAMGB: 24, Perf: 83, MaxGPU: 8},
		{Name: "5090", VRAMGB: 32, Perf: 105, MaxGPU: 8},
		{Name: "A100", VRAMGB: 80, Perf: 100, MaxGPU: 8},
	}

	// 7B (≈17GB) → smallest available that fits = 4090 (24GB).
	gt, _ := RecommendGPUTypeLive("Qwen2.5-7B", "fp16", "部署", nil, live)
	assert.Equal(t, "4090", gt)

	// 32B (≈77GB) → smallest available ≥77 = A100 (80GB).
	gt, _ = RecommendGPUTypeLive("Qwen2.5-32B", "fp16", "部署", nil, live)
	assert.Equal(t, "A100", gt)

	// SOLD-OUT EXCLUSION: 4090 not in the live set → 7B sizes up to 5090 (32GB),
	// not the now-unavailable 4090. The static table would still pick 4090.
	liveNo4090 := []AvailableGPU{
		{Name: "5090", VRAMGB: 32, Perf: 105, MaxGPU: 8},
		{Name: "A100", VRAMGB: 80, Perf: 100, MaxGPU: 8},
	}
	gt, _ = RecommendGPUTypeLive("Qwen2.5-7B", "fp16", "部署", nil, liveNo4090)
	assert.Equal(t, "5090", gt, "sold-out 4090 excluded → next available tier")

	// NEW CARD the static table never models: 16B (≈39GB) → the only fitting card
	// is a hypothetical "B200" (48GB). Static would pick 4090_48G; live picks B200.
	liveNew := []AvailableGPU{
		{Name: "4090", VRAMGB: 24, Perf: 83, MaxGPU: 8},
		{Name: "B200", VRAMGB: 48, Perf: 130, MaxGPU: 8},
		{Name: "A100", VRAMGB: 80, Perf: 100, MaxGPU: 8},
	}
	gt, _ = RecommendGPUTypeLive("Qwen2.5-16B", "fp16", "部署", nil, liveNew)
	assert.Equal(t, "B200", gt, "a new card absent from the static table is selectable live")

	// EMPTY live set → static-table fallback (7B → 4090).
	gt, _ = RecommendGPUTypeLive("Qwen2.5-7B", "fp16", "部署", nil, nil)
	assert.Equal(t, "4090", gt, "empty live set degrades to static RecommendGPUTypeWithin")

	// M2 ∩ live: image supports only 5090 → 4090-ideal overridden to the supported,
	// available, fitting 5090.
	gt, note := RecommendGPUTypeLive("Qwen2.5-7B", "fp16", "部署", []string{"5090"}, live)
	assert.Equal(t, "5090", gt)
	assert.Contains(t, note, "5090")

	// Multi-card: 70B (≈168GB) exceeds any single live card → largest available
	// (A100), split across multiple cards (168/80 → 3×A100).
	gt, note = RecommendGPUTypeLive("Llama3-70B", "fp16", "部署", nil, live)
	assert.Equal(t, "A100", gt)
	assert.Contains(t, note, "无单卡可承载")
	assert.Contains(t, note, "×A100")

	// SAME-VRAM-TIER tiebreak (e.g. A100 vs A800, both 80GB): pinned deterministic.
	// Equal live Perf → lexicographically smaller Name; unequal → higher Perf wins.
	// (Unlike the static table's FP16 tiebreak, the live path uses live Perf, so it
	// picks the faster offered card in a tier — A100/A800 are equivalent for sizing.)
	tieEqual := []AvailableGPU{
		{Name: "A800", VRAMGB: 80, Perf: 312, MaxGPU: 8},
		{Name: "A100", VRAMGB: 80, Perf: 312, MaxGPU: 8},
	}
	gt, _ = RecommendGPUTypeLive("Qwen2.5-32B", "fp16", "部署", nil, tieEqual)
	assert.Equal(t, "A100", gt, "equal-perf 80GB tie → lexicographically smaller name (deterministic)")

	tiePerf := []AvailableGPU{
		{Name: "A100", VRAMGB: 80, Perf: 312, MaxGPU: 8},
		{Name: "A800", VRAMGB: 80, Perf: 320, MaxGPU: 8},
	}
	gt, _ = RecommendGPUTypeLive("Qwen2.5-32B", "fp16", "部署", nil, tiePerf)
	assert.Equal(t, "A800", gt, "higher live perf wins within a VRAM tier")
}
