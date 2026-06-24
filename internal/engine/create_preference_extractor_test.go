package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCreatePreferenceExtraction(t *testing.T) {
	tests := []struct {
		name string
		json string
		want CreatePreferenceExtractionResult
	}{
		{
			name: "pytorch image preference",
			json: `{"workload_pref":"","image_pref":"PyTorch","image_source":"","gpu_pref":"4090","zone_pref":"","purpose":"deep_learning"}`,
			want: CreatePreferenceExtractionResult{ImagePref: "PyTorch", GPUPref: "4090", Purpose: "deep_learning"},
		},
		{
			name: "community digital human workload",
			json: `{"workload_pref":"数字人","image_pref":"","image_source":"社区镜像","gpu_pref":"","zone_pref":"","purpose":"community"}`,
			want: CreatePreferenceExtractionResult{WorkloadPref: "数字人", ImageSource: "community", Purpose: "community"},
		},
		{
			name: "deepseek workload not image",
			json: `{"workload_pref":"DeepSeek R1 32B","image_pref":"","image_source":"","gpu_pref":"","zone_pref":"","purpose":"llm_inference"}`,
			want: CreatePreferenceExtractionResult{WorkloadPref: "DeepSeek R1 32B", Purpose: "llm_inference"},
		},
		{
			name: "no hallucinated image",
			json: `{"workload_pref":"","image_pref":"","image_source":"","gpu_pref":"4090","zone_pref":"","purpose":""}`,
			want: CreatePreferenceExtractionResult{GPUPref: "4090"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCreatePreferenceExtraction(tc.json)

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestDeployMessageWithCreatePreferenceUsesWorkloadAsTarget(t *testing.T) {
	got := deployMessageWithCreatePreference("32B", CreatePreferenceExtractionResult{
		WorkloadPref: "DeepSeek R1 32B",
		ImageSource:  "community",
	})

	assert.Contains(t, got, "部署目标：DeepSeek R1 32B")
	assert.Contains(t, got, "镜像来源：community")
	assert.NotContains(t, got, "镜像偏好：DeepSeek R1 32B")
}

func TestMergeCreatePreferenceArgsDoesNotOverwriteExistingGPUCount(t *testing.T) {
	eng := NewWithDeps(nil, nil, nil)

	got := eng.mergeCreatePreferenceArgs(context.Background(), map[string]any{
		"GpuType": "4090",
		"Gpu":     float64(2),
		"Zone":    "cn-wlcb-01",
	}, CreatePreferenceExtractionResult{
		GPUPref:   "4090",
		ZonePref:  "华北一C",
		ImagePref: "PyTorch 最新镜像",
	})

	assert.Equal(t, "4090", got["GpuType"])
	assert.Equal(t, float64(2), got["Gpu"])
	assert.Equal(t, "cn-wlcb-01", got["Zone"])
	assert.Equal(t, "torch", got["ImageName"])
}

func TestMergeCreatePreferenceArgsLetsLLMOverrideImageHints(t *testing.T) {
	eng := NewWithDeps(nil, nil, nil)

	got := eng.mergeCreatePreferenceArgs(context.Background(), map[string]any{
		"GpuType":     "4090",
		"Gpu":         float64(1),
		"ImageName":   "torch",
		"ImageSource": "platform",
	}, CreatePreferenceExtractionResult{
		ImagePref:   "ComfyUI",
		ImageSource: "community",
		Purpose:     "community",
	})

	assert.Equal(t, "4090", got["GpuType"])
	assert.Equal(t, float64(1), got["Gpu"])
	assert.Equal(t, "ComfyUI", got["ImageName"])
	assert.Equal(t, "community", got["ImageSource"])
	assert.Equal(t, "community", got["ImagePurpose"])
}
