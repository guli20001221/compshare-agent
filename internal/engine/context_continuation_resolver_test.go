package engine

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/intent"
)

func TestParseContextContinuationDecision_ContinueKeepsExplicitFields(t *testing.T) {
	decision, err := parseContextContinuationDecision(`{
		"decision":"continue_task",
		"gpu_pref":"5090",
		"zone_pref":"华北二A",
		"image_pref":"PyTorch",
		"image_source":"社区镜像",
		"workload_pref":"Qwen 32B"
	}`)

	require.NoError(t, err)
	require.NotNil(t, decision)
	assert.Equal(t, ContextContinuationContinue, decision.Decision)
	assert.Equal(t, "5090", decision.GPUPref)
	assert.Equal(t, "华北二A", decision.ZonePref)
	assert.Equal(t, "PyTorch", decision.ImagePref)
	assert.Equal(t, "community", decision.ImageSource)
	assert.Equal(t, "Qwen 32B", decision.WorkloadPref)
}

func TestParseContextContinuationDecision_NewQuestionDropsStaleFields(t *testing.T) {
	decision, err := parseContextContinuationDecision(`{
		"decision":"new_question",
		"gpu_pref":"5090",
		"zone_pref":"华北二A",
		"image_pref":"PyTorch",
		"image_source":"community",
		"workload_pref":"Qwen 32B"
	}`)

	require.NoError(t, err)
	require.NotNil(t, decision)
	assert.Equal(t, ContextContinuationNew, decision.Decision)
	assert.Empty(t, decision.GPUPref)
	assert.Empty(t, decision.ZonePref)
	assert.Empty(t, decision.ImagePref)
	assert.Empty(t, decision.ImageSource)
	assert.Empty(t, decision.WorkloadPref)
}

func TestBuildContextContinuationPromptIncludesFrameAndInstanceSummary(t *testing.T) {
	msgs := buildContextContinuationPrompt(ContextContinuationInput{
		UserText:     "那华北二A呢",
		RouterIntent: intent.IntentStockAvailability,
		ContextFrame: ContextFrame{
			Kind:            ContextFrameKindDeploy,
			Status:          ContextFrameStatusFailedRecoverable,
			GPU:             "4090",
			ImagePref:       "PyTorch",
			ZoneLabel:       "华北一C",
			FailureReason:   "华北一C 暂无库存",
			OriginalUserMsg: "在华北一C用最新pytorch给我开一台4090",
		},
		InstanceContext: "selected_instance: train-a (uhost-1)",
	})

	require.Len(t, msgs, 2)
	userPrompt := msgs[1].Content
	assert.True(t, strings.Contains(userPrompt, "router_intent: stock_availability"))
	assert.True(t, strings.Contains(userPrompt, "gpu: 4090"))
	assert.True(t, strings.Contains(userPrompt, "image_pref: PyTorch"))
	assert.True(t, strings.Contains(userPrompt, "zone_label: 华北一C"))
	assert.True(t, strings.Contains(userPrompt, "selected_instance: train-a"))
}
