package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
)

func TestParseCreatePreferenceExtraction_KeepsEmptyFieldsEmpty(t *testing.T) {
	got, err := parseCreatePreferenceExtraction(`{
		"workload_pref": " DeepSeek R1 32B ",
		"image_pref": "",
		"image_source": "",
		"gpu_pref": " 4090 ",
		"zone_pref": " 华北一C ",
		"purpose": ""
	}`)

	require.NoError(t, err)
	assert.Equal(t, "DeepSeek R1 32B", got.WorkloadPref)
	assert.Equal(t, "", got.ImagePref)
	assert.Equal(t, "", got.ImageSource)
	assert.Equal(t, "4090", got.GPUPref)
	assert.Equal(t, "华北一C", got.ZonePref)
	assert.Equal(t, "", got.Purpose)
}

func TestParseCreatePreferenceExtraction_DropsUnknownEnums(t *testing.T) {
	got, err := parseCreatePreferenceExtraction(`{
		"workload_pref": "数字人",
		"image_pref": "LiveTalking",
		"image_source": "whatever",
		"gpu_pref": "",
		"zone_pref": "",
		"purpose": "guess-from-workload"
	}`)

	require.NoError(t, err)
	assert.Equal(t, "数字人", got.WorkloadPref)
	assert.Equal(t, "LiveTalking", got.ImagePref)
	assert.Equal(t, "", got.ImageSource)
	assert.Equal(t, "", got.Purpose)
}

func TestLLMCreatePreferenceExtractor_DeepSeekDoesNotInferPurposeOrImageSource(t *testing.T) {
	client := &mockLLM{responses: []llm.ChatResponse{{
		Content: `{"workload_pref":"DeepSeek R1 32B","image_pref":"","image_source":"","gpu_pref":"","zone_pref":"","purpose":""}`,
	}}}
	extractor := &llmCreatePreferenceExtractor{client: client}

	got, err := extractor.ExtractCreatePreferences(context.Background(), CreatePreferenceExtractionInput{
		UserText: "部署 DeepSeek R1 32B",
		Intent:   intent.IntentDeployModel,
	})

	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "DeepSeek R1 32B", got.WorkloadPref)
	assert.Equal(t, "", got.ImagePref)
	assert.Equal(t, "", got.ImageSource)
	assert.Equal(t, "", got.Purpose)
	require.Len(t, client.calls, 1)
	assert.NotContains(t, joinMessageContent(client.calls[0]), "speech_act", "Option B must not depend on PR342 speech_act")
	assert.Contains(t, joinMessageContent(client.calls[0]), "不得因为用户要部署某个模型就推断 purpose 或 image_source")
}

func TestLLMCreatePreferenceExtractor_DropsModelInferredPurposeAndImageSource(t *testing.T) {
	client := &mockLLM{responses: []llm.ChatResponse{{
		Content: `{"workload_pref":"DeepSeek R1 32B","image_pref":"","image_source":"platform","gpu_pref":"","zone_pref":"","purpose":"inference"}`,
	}}}
	extractor := &llmCreatePreferenceExtractor{client: client}

	got, err := extractor.ExtractCreatePreferences(context.Background(), CreatePreferenceExtractionInput{
		UserText: "部署 DeepSeek R1 32B",
		Intent:   intent.IntentDeployModel,
	})

	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "DeepSeek R1 32B", got.WorkloadPref)
	assert.Equal(t, "", got.ImageSource, "image_source must be explicit in the user text, not inferred from model deploy")
	assert.Equal(t, "", got.Purpose, "purpose must be explicit in the user text, not inferred from model deploy")
}

func TestLLMCreatePreferenceExtractor_KeepsExplicitPurposeAndImageSource(t *testing.T) {
	client := &mockLLM{responses: []llm.ChatResponse{{
		Content: `{"workload_pref":"数字人","image_pref":"","image_source":"community","gpu_pref":"","zone_pref":"","purpose":"training"}`,
	}}}
	extractor := &llmCreatePreferenceExtractor{client: client}

	got, err := extractor.ExtractCreatePreferences(context.Background(), CreatePreferenceExtractionInput{
		UserText: "用社区镜像训练一个数字人",
		Intent:   intent.IntentDeployModel,
	})

	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "community", got.ImageSource)
	assert.Equal(t, "training", got.Purpose)
}

func TestDeployMessageWithCreatePreference_AppendsOnlyNonEmptyPreferences(t *testing.T) {
	msg := deployMessageWithCreatePreference("部署 DeepSeek R1 32B", CreatePreferenceExtractionResult{
		WorkloadPref: "DeepSeek R1 32B",
		ImagePref:    "PyTorch",
		GPUPref:      "4090",
	})

	assert.Contains(t, msg, "用户原话：部署 DeepSeek R1 32B")
	assert.Contains(t, msg, "workload_pref: DeepSeek R1 32B")
	assert.Contains(t, msg, "image_pref: PyTorch")
	assert.Contains(t, msg, "gpu_pref: 4090")
	assert.NotContains(t, msg, "image_source:")
	assert.NotContains(t, msg, "purpose:")
}

func joinMessageContent(req llm.ChatRequest) string {
	var parts []string
	for _, m := range req.Messages {
		parts = append(parts, m.Content)
	}
	return strings.Join(parts, "\n")
}
