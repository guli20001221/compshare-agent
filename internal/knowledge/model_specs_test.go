package knowledge

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestResolveParamCountB pins the size-ambiguity contract. It is the same
// contract TestModelParamCountResolvable used to assert; that test drove the
// exported ModelParamCountResolvable wrapper, which had ZERO production callers
// (its doc claimed "the deploy clarify gate reads" it — the caller was deleted
// long ago and the comment went stale). The wrapper is gone; the contract is
// NOT, because FittingGPUAlternatives branches on resolveParamCountB to decide
// whether a VRAM floor applies at all.
//
// The load-bearing half is the FALSE cases: a bare multi-size family name must
// stay unresolved. If "DeepSeek R1" (1.5B–671B) silently resolved to one total,
// FittingGPUAlternatives would impose a VRAM floor for a config the user never
// asked for and filter out cards that are in fact fine.
func TestResolveParamCountB(t *testing.T) {
	// Resolvable: explicit size token, or a single-variant canonical entry.
	for name, want := range map[string]float64{
		"Qwen2.5-32B":                 32,
		"DeepSeek-R1-Distill-Qwen-7B": 7,
		"mixtral-8x7b":                47, // canonical MoE total, NOT the "7b" the regex sees
		"QwQ":                         32, // single variant
		"qwen3-32b":                   32, // last <n>b token wins, not the "3" in qwen3
	} {
		got, ok := resolveParamCountB(name)
		assert.Truef(t, ok, "resolveParamCountB(%q) should resolve", name)
		assert.Equalf(t, want, got, "resolveParamCountB(%q)", name)
	}

	// Ambiguous: multi-size family with no size, or a non-model app name.
	for _, name := range []string{"DeepSeek R1", "deepseek-v3", "ComfyUI", ""} {
		_, ok := resolveParamCountB(name)
		assert.Falsef(t, ok, "resolveParamCountB(%q) must stay unresolved rather than guess a size", name)
	}
}

func TestNormalizeModelName(t *testing.T) {
	for _, in := range []string{"DeepSeek-V3", "deepseek v3", "deepseek_v3", "DeepSeek.V3"} {
		assert.Equalf(t, "deepseekv3", normalizeModelName(in), "normalizeModelName(%q)", in)
	}
}
