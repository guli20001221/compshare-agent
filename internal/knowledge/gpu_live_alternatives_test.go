package knowledge

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func gpuAltNames(gs []AvailableGPU) []string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.Name)
	}
	return out
}

func TestFittingGPUAlternatives(t *testing.T) {
	cards := []AvailableGPU{
		{Name: "4090", VRAMGB: 24, Perf: 100},
		{Name: "5090", VRAMGB: 32, Perf: 120},
		{Name: "4090_48G", VRAMGB: 48, Perf: 110},
		{Name: "A800", VRAMGB: 80, Perf: 150},
		{Name: "2080", VRAMGB: 8, Perf: 50},
	}

	// A 7B model (~17GB) was sized onto 4090; it's now sold out. The alternatives
	// must drop the too-small 2080, exclude the sold-out 4090, and rank the rest
	// cheapest-(smallest-VRAM)-first so the user is offered the least over-provisioned
	// option first.
	require.Equal(t, []string{"5090", "4090_48G", "A800"},
		gpuAltNames(FittingGPUAlternatives("Qwen2.5-7B", "fp16", nil, cards, "4090", 3)))

	// limit caps the list.
	require.Equal(t, []string{"5090", "4090_48G"},
		gpuAltNames(FittingGPUAlternatives("Qwen2.5-7B", "fp16", nil, cards, "4090", 2)))

	// image-compat: when the image's supported set intersects what's offered, only
	// compatible cards (that also fit) are suggested — never an incompatible card.
	require.Equal(t, []string{"5090", "A800"},
		gpuAltNames(FittingGPUAlternatives("Qwen2.5-7B", "fp16", []string{"A800", "5090"}, cards, "4090", 3)))

	// Unknown model size → nil (no VRAM math possible) so the caller degrades to a
	// generic "换一个规格" message rather than suggesting random cards.
	require.Nil(t, FittingGPUAlternatives("mystery-model", "fp16", nil, cards, "4090", 3))
}
