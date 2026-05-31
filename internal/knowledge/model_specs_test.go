package knowledge

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func recommended(t *testing.T, res map[string]any) gpuFit {
	t.Helper()
	require.True(t, res["resolved"].(bool), "expected resolved=true: %+v", res)
	r, ok := res["recommended"].(gpuFit)
	require.True(t, ok, "recommended must be a gpuFit: %T", res["recommended"])
	return r
}

// TestRecommendGPUType pins the typed accessor the deploy_model arm calls
// (B8.3): a recognized model name sizes by VRAM (same pick as
// GetModelVRAMRequirement); an unrecognized name falls back to the scene
// recommendation; it always returns a non-empty, valid GpuType.
func TestRecommendGPUType(t *testing.T) {
	// Model-name path: Qwen32B → A100 (smallest single card ≥ 77GB), matching
	// GetModelVRAMRequirement's recommendation.
	gt, note := RecommendGPUType("Qwen32B", "fp16", "部署 Qwen32B")
	assert.Equal(t, "A100", gt)
	assert.NotEmpty(t, note)

	// Quantization shrinks the footprint: 32B int4 = 32×0.5×1.2 = 19.2 → ≥24GB.
	gt, _ = RecommendGPUType("Qwen32B", "int4", "部署")
	if _, ok := gpuSpecs[gt]; !ok {
		t.Errorf("int4 pick %q not a known GPU type", gt)
	}
	assert.GreaterOrEqual(t, gpuSpecs[gt].VRAM, 24)

	// Scene fallback: no model name → GetGPURecommendation by scene keyword.
	gt, _ = RecommendGPUType("", "", "推理部署")
	require.NotEmpty(t, gt)
	_, ok := gpuSpecs[gt]
	assert.True(t, ok, "scene pick %q must be a known GPU type", gt)

	// Total fallback: unrecognizable input still returns a usable GPU.
	gt, _ = RecommendGPUType("", "", "")
	require.NotEmpty(t, gt)
	_, ok = gpuSpecs[gt]
	assert.True(t, ok)
}

// TestGetModelVRAMRequirement_FP16SingleCard pins the headline B8 case: a 32B
// model in FP16 needs ~77GB (32×2×1.2), which the smallest single card that
// fits is the A100 (80GB). This is the reasoning deploy_model uses to pick GpuType.
func TestGetModelVRAMRequirement_FP16SingleCard(t *testing.T) {
	res := GetModelVRAMRequirement("Qwen32B", "fp16")
	assert.Equal(t, float64(32), res["params_b"])
	assert.Equal(t, "fp16", res["quantization"])
	assert.Equal(t, 77, res["vram_required_gb"]) // ceil(32*2*1.2)=ceil(76.8)
	rec := recommended(t, res)
	assert.Equal(t, "A100", rec.GPUType, "smallest single card >= 77GB")
	assert.Equal(t, 1, rec.Cards)
	// the option list is ascending by VRAM and only contains cards that fit
	opts := res["single_card_options"].([]gpuFit)
	require.NotEmpty(t, opts)
	assert.Equal(t, "A100", opts[0].GPUType)
	for _, o := range opts {
		assert.GreaterOrEqual(t, o.VRAMGB, 77)
	}
}

func TestGetModelVRAMRequirement_SmallModel(t *testing.T) {
	res := GetModelVRAMRequirement("7B", "fp16")
	assert.Equal(t, float64(7), res["params_b"])
	assert.Equal(t, 17, res["vram_required_gb"]) // ceil(7*2*1.2)=ceil(16.8)
	rec := recommended(t, res)
	// smallest tier >=17 is 24GB; best FP16 at 24GB (deterministic tiebreak) = 4090
	assert.Equal(t, "4090", rec.GPUType)
	assert.Equal(t, 24, rec.VRAMGB)
}

func TestGetModelVRAMRequirement_Quantization(t *testing.T) {
	int8 := GetModelVRAMRequirement("Qwen32B", "int8")
	assert.Equal(t, float64(1), int8["bytes_per_param"])
	assert.Equal(t, 39, int8["vram_required_gb"]) // ceil(32*1*1.2)=ceil(38.4)
	assert.Equal(t, "4090_48G", recommended(t, int8).GPUType, "int8 fits a 48GB card")

	int4 := GetModelVRAMRequirement("Qwen32B", "int4")
	assert.Equal(t, 20, int4["vram_required_gb"]) // ceil(32*0.5*1.2)=ceil(19.2)
	assert.Equal(t, "4090", recommended(t, int4).GPUType, "int4 fits a 24GB card")
}

func TestGetModelVRAMRequirement_UnknownQuantizationDefaultsFP16(t *testing.T) {
	res := GetModelVRAMRequirement("Qwen32B", "fp7-nonsense")
	assert.Equal(t, "fp16", res["quantization"])
	assert.Equal(t, float64(2), res["bytes_per_param"])
}

func TestGetModelVRAMRequirement_MultiCardFallback(t *testing.T) {
	res := GetModelVRAMRequirement("Llama3-70B", "fp16")
	assert.Equal(t, float64(70), res["params_b"])
	assert.Equal(t, 168, res["vram_required_gb"]) // ceil(70*2*1.2)
	assert.Empty(t, res["single_card_options"].([]gpuFit), "no single card holds 168GB")
	rec := recommended(t, res)
	assert.Equal(t, "H20", rec.GPUType, "largest card used for the multi-card split")
	assert.Equal(t, 2, rec.Cards) // ceil(168/96)
	assert.Equal(t, 192, rec.TotalVRAMGB)
	mc, ok := res["multi_card_fallback"].(gpuFit)
	require.True(t, ok)
	assert.Equal(t, 2, mc.Cards)
}

func TestGetModelVRAMRequirement_UnresolvedName(t *testing.T) {
	res := GetModelVRAMRequirement("SomeBrandNewModel", "fp16")
	assert.False(t, res["resolved"].(bool))
	assert.Contains(t, res["note"].(string), "参数量")
	assert.Nil(t, res["recommended"])
}

func TestGetModelVRAMRequirement_DecimalParams(t *testing.T) {
	res := GetModelVRAMRequirement("Qwen2.5-1.5B-Instruct", "fp16")
	assert.Equal(t, float64(1.5), res["params_b"])
	assert.Equal(t, 4, res["vram_required_gb"]) // ceil(1.5*2*1.2)=ceil(3.6)
	assert.Equal(t, "2080", recommended(t, res).GPUType, "smallest tier (8GB) holds a 1.5B model")
}

func TestGetModelVRAMRequirement_LastTokenWins(t *testing.T) {
	// "qwen3-32b": the "3" must not be read as the param count — the trailing
	// "32b" token wins (it's the one followed by 'b').
	res := GetModelVRAMRequirement("qwen3-32b", "fp16")
	assert.Equal(t, float64(32), res["params_b"])
}

func TestGetModelVRAMRequirement_CanonicalMoE(t *testing.T) {
	// mixtral-8x7b: regex would wrongly read "7b" → 7; the canonical table
	// gives the correct MoE total (47B, all experts must be VRAM-resident).
	res := GetModelVRAMRequirement("mixtral-8x7b", "fp16")
	assert.Equal(t, float64(47), res["params_b"])

	// deepseek-v3 has no count in the name → canonical 671B → far beyond a
	// single host's card limit, surfaced in the note.
	ds := GetModelVRAMRequirement("deepseek-v3", "fp16")
	assert.Equal(t, float64(671), ds["params_b"])
	rec := recommended(t, ds)
	assert.Greater(t, rec.Cards, 8)
	assert.Contains(t, rec.Note, "超过单机")
}

func TestGetModelVRAMRequirement_ViaExecuteTool(t *testing.T) {
	res, err := ExecuteTool("GetModelVRAMRequirement", map[string]any{"model_name": "Qwen32B"})
	require.NoError(t, err)
	assert.Equal(t, float64(32), res["params_b"])
	assert.Equal(t, "fp16", res["quantization"]) // default when quantization arg omitted
	assert.True(t, IsKnowledgeTool("GetModelVRAMRequirement"))
}
