package knowledge

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetGPUSpecs_SingleGPU(t *testing.T) {
	result, err := GetGPUSpecs("4090")
	assert.NoError(t, err)

	spec := result["spec"].(GPUSpec)
	assert.Equal(t, "RTX 4090", spec.Name)
	assert.Equal(t, 24, spec.VRAM)
	assert.Equal(t, 82.6, spec.FP16)
	assert.Equal(t, 10, spec.MaxGPU)
	assert.True(t, spec.SpotSupport)
}

func TestGetGPUSpecs_A100(t *testing.T) {
	result, err := GetGPUSpecs("A100")
	assert.NoError(t, err)

	spec := result["spec"].(GPUSpec)
	assert.Equal(t, "A100 80GB", spec.Name)
	assert.Equal(t, 80, spec.VRAM)
	assert.Equal(t, 312.0, spec.FP16)
}

func TestGetGPUSpecs_Unknown(t *testing.T) {
	_, err := GetGPUSpecs("TITAN")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "未知的 GPU 类型")
}

func TestGetGPUSpecs_All(t *testing.T) {
	result, err := GetGPUSpecs("")
	assert.NoError(t, err)

	specs := result["gpu_specs"]
	assert.NotNil(t, specs)

	// Verify via JSON round-trip that all GPU types are present
	jsonStr := ResultToJSON(result)
	assert.Contains(t, jsonStr, "4090")
	assert.Contains(t, jsonStr, "A100")
	assert.Contains(t, jsonStr, "H20")
	assert.Contains(t, jsonStr, "3090")
}

func TestGetGPURecommendation_Inference(t *testing.T) {
	result := GetGPURecommendation("推理部署vLLM", false)

	recs, ok := result["recommendations"].([]GPURec)
	assert.True(t, ok)
	assert.NotEmpty(t, recs)

	// First recommendation for inference should be 4090
	assert.Equal(t, "4090", recs[0].GPUType)
}

func TestGetGPURecommendation_Training(t *testing.T) {
	result := GetGPURecommendation("全量训练大模型", false)

	recs := result["recommendations"].([]GPURec)
	assert.NotEmpty(t, recs)

	// First recommendation for full training should be A100
	assert.Equal(t, "A100", recs[0].GPUType)
}

func TestGetGPURecommendation_BudgetSensitive(t *testing.T) {
	normal := GetGPURecommendation("推理", false)
	budget := GetGPURecommendation("推理", true)

	normalRecs := normal["recommendations"].([]GPURec)
	budgetRecs := budget["recommendations"].([]GPURec)

	// Budget sensitive should reverse the order
	assert.Equal(t, normalRecs[0].GPUType, budgetRecs[len(budgetRecs)-1].GPUType)
}

func TestGetGPURecommendation_SD(t *testing.T) {
	result := GetGPURecommendation("跑stable diffusion绘图", false)

	recs := result["recommendations"].([]GPURec)
	assert.NotEmpty(t, recs)
	assert.Equal(t, "4090", recs[0].GPUType)
}

func TestGetGPURecommendation_Beginner(t *testing.T) {
	result := GetGPURecommendation("学生学习入门", false)

	recs := result["recommendations"].([]GPURec)
	assert.NotEmpty(t, recs)
	// Should recommend budget-friendly options
	assert.Equal(t, "3080Ti", recs[0].GPUType)
}

func TestGetGPURecommendation_LoRA(t *testing.T) {
	result := GetGPURecommendation("LoRA微调7B模型", false)

	recs := result["recommendations"].([]GPURec)
	assert.NotEmpty(t, recs)
	assert.Equal(t, "4090", recs[0].GPUType)
}

func TestGetGPURecommendation_UnknownScene(t *testing.T) {
	result := GetGPURecommendation("量子计算模拟", false)

	recs := result["recommendations"].([]GPURec)
	assert.NotEmpty(t, recs)
	// Default path should recommend 4090 first (general purpose)
	assert.Equal(t, "4090", recs[0].GPUType)
}

func TestGetGPUSpecs_V100S_DenseNotSparsity(t *testing.T) {
	result, err := GetGPUSpecs("V100S")
	assert.NoError(t, err)
	spec := result["spec"].(GPUSpec)
	// V100S Tensor Core FP16 dense = 32.8 TFLOPS (not 130 which is with Tensor sparsity)
	assert.Equal(t, 32.8, spec.FP16)
}

func TestGetGPUSpecs_H100_H200_NotAdvertised(t *testing.T) {
	// H100 and H200 are NOT available on CompShare (upstream gpu.go only
	// defines H20). Guard against accidental re-introduction of ghost GPUs
	// that would lead the agent to recommend unbuyable hardware.
	for _, gt := range []string{"H100", "H200"} {
		_, err := GetGPUSpecs(gt)
		assert.Error(t, err, "%s must not be in gpuSpecs; platform does not sell it", gt)
	}
}

func TestGetGPURecommendation_FullTraining_NoH100(t *testing.T) {
	// Full-training recommendation must not include H100 — the platform
	// only sells H20 in the Hopper tier.
	result := GetGPURecommendation("全量训练", false)
	recs := result["recommendations"].([]GPURec)
	for _, r := range recs {
		assert.NotEqual(t, "H100", r.GPUType, "H100 must not appear in full-training recs")
		assert.NotEqual(t, "H200", r.GPUType, "H200 must not appear in full-training recs")
	}
}

func TestGetGPUSpecs_4090_ConservativeMaxCPU(t *testing.T) {
	// Upstream has G_AMD_4090 (MaxCPU=128) and G_INTEL_4090 (MaxCPU=140).
	// We report the AMD lower bound so users don't over-plan.
	result, err := GetGPUSpecs("4090")
	assert.NoError(t, err)
	spec := result["spec"].(GPUSpec)
	assert.Equal(t, 128, spec.MaxCPU, "4090 MaxCPU must match AMD lower bound, not Intel upper bound")
}

func TestGetGPUSpecs_5090_Estimated(t *testing.T) {
	result, err := GetGPUSpecs("5090")
	assert.NoError(t, err)
	spec := result["spec"].(GPUSpec)
	assert.Equal(t, 105.0, spec.FP16)
	assert.Equal(t, 32, spec.VRAM)
}

func TestExecuteTool_GetGPUSpecs(t *testing.T) {
	result, err := ExecuteTool("GetGPUSpecs", map[string]any{
		"GpuType": "H20",
	})
	assert.NoError(t, err)

	spec := result["spec"].(GPUSpec)
	assert.Equal(t, 96, spec.VRAM)
	assert.Equal(t, 148.0, spec.FP16)
}

func TestExecuteTool_GetGPURecommendation(t *testing.T) {
	result, err := ExecuteTool("GetGPURecommendation", map[string]any{
		"scene":            "训练7B模型",
		"budget_sensitive": false,
	})
	assert.NoError(t, err)
	assert.NotNil(t, result["recommendations"])
}

func TestGetGPURecommendation_BuildRecsInvalidGPU(t *testing.T) {
	// buildRecs silently skips unknown GPU types — verify no panic
	result := GetGPURecommendation("推理", false)
	recs := result["recommendations"].([]GPURec)
	for _, r := range recs {
		_, err := GetGPUSpecs(r.GPUType)
		assert.NoError(t, err, "recommended GPU %s should exist in spec table", r.GPUType)
	}
}

func TestContainsAny_EdgeCases(t *testing.T) {
	assert.False(t, containsAny("", "test"))
	assert.True(t, containsAny("hello", ""))          // Go: strings.Contains(s, "") is always true
	assert.True(t, containsAny("推理部署", "推理"))
	assert.True(t, containsAny("lora微调", "lora"))    // caller must pre-lowercase s
	assert.False(t, containsAny("LORA微调", "lora"))   // uppercase s won't match (caller's job)
	assert.False(t, containsAny("training", "推理"))
}

func TestExecuteTool_Unknown(t *testing.T) {
	_, err := ExecuteTool("UnknownTool", map[string]any{})
	assert.Error(t, err)
}

func TestIsKnowledgeTool(t *testing.T) {
	assert.True(t, IsKnowledgeTool("GetGPUSpecs"))
	assert.True(t, IsKnowledgeTool("GetGPURecommendation"))
	assert.False(t, IsKnowledgeTool("DescribeCompShareInstance"))
	assert.False(t, IsKnowledgeTool(""))
}

func TestResultToJSON(t *testing.T) {
	result := map[string]any{"key": "value", "num": 42}
	json := ResultToJSON(result)
	assert.Contains(t, json, `"key":"value"`)
	assert.Contains(t, json, `"num":42`)
}
