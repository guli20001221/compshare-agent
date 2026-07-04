package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/intent"
	openai "github.com/sashabaranov/go-openai"
)

type fakeContextDecisionLayer struct {
	decision *ContextDecision
	err      error
	calls    []ContextDecisionInput
}

func (f *fakeContextDecisionLayer) ResolveContextDecision(_ context.Context, in ContextDecisionInput) (*ContextDecision, error) {
	f.calls = append(f.calls, in)
	if f.err != nil {
		return nil, f.err
	}
	return f.decision, nil
}

func TestBuildContextDecisionPromptIncludesTaskEntityChoicesAndFacts(t *testing.T) {
	now := time.Unix(1716530100, 0)
	input := buildContextDecisionInput(SessionState{
		SchemaVersion:        SessionStateSchemaCurrent,
		SelectedInstanceID:   "uhost-selected",
		SelectedInstanceName: "train-a",
		PendingSelectionKind: pendingSelectionKindInstance,
		PendingSelectionItems: []PendingSelectionItem{
			{Index: 1, ID: "uhost-1", Name: "first", State: "Running", GpuType: "4090", GPU: 1},
			{Index: 2, ID: "uhost-2", Name: "second", State: "Stopped", GpuType: "A100", GPU: 1},
		},
		ContextFrame: ContextFrame{
			Version:         1,
			Kind:            ContextFrameKindDeploy,
			Status:          ContextFrameStatusFailedRecoverable,
			GPU:             "4090",
			ImagePref:       "PyTorch",
			ZoneLabel:       "华北一C",
			FailureReason:   "华北一C 暂无库存",
			ProducedAtUnix:  now.Unix(),
			TTLSeconds:      ContextFrameTTLSeconds,
			OriginalUserMsg: "在华北一C用最新pytorch给我开一台4090",
		},
		RecentFacts: []ToolFact{{
			Kind:           FactKindMonitorSample,
			SubjectID:      "uhost-selected",
			Payload:        map[string]any{"gpu_usage": "88.0"},
			ProducedAtTurn: 3,
			ProducedAtUnix: now.Unix(),
			TTLSeconds:     factTTLSecondsMonitorSample,
		}},
	}, "第1台 GPU 忙不忙", intent.IntentMonitorQuery, now)

	msgs := buildContextDecisionPrompt(input)

	require.Len(t, msgs, 2)
	promptText := msgs[1].Content
	for _, want := range []string{
		"router_intent: monitor_query",
		"active_task:",
		"gpu: 4090",
		"selected_entity:",
		"selected_instance: train-a (uhost-selected)",
		"pending_choices:",
		"1:first(uhost-1)",
		"recent_facts:",
		"监控 GPU 88.0%",
		"user_text: 第1台 GPU 忙不忙",
	} {
		assert.Contains(t, promptText, want)
	}
	assert.NotContains(t, promptText, "zone_id")
	assert.NotContains(t, promptText, "az_group")
}

func TestEngineBuildContextDecisionInputIncludesLastAssistantPrompt(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleAssistant, Content: "请补充数据盘大小，例如 200G。"},
	}

	input := eng.buildContextDecisionInput("200G", intent.IntentOperationLifecycle, ContextFrame{
		Kind:     ContextFrameKindWorkflowTask,
		Workflow: "CreateDiskWorkflow",
	}, time.Unix(1716530100, 0))

	assert.Equal(t, "请补充数据盘大小，例如 200G。", input.LastAssistantPrompt)
	msgs := buildContextDecisionPrompt(input)
	require.Len(t, msgs, 2)
	assert.Contains(t, msgs[1].Content, "last_assistant_prompt:")
	assert.Contains(t, msgs[1].Content, "请补充数据盘大小")
}

func TestParseContextDecisionSelectEntityKeepsInstanceReferenceOnly(t *testing.T) {
	decision, err := parseContextDecision(`{
		"decision":"select_entity",
		"target":"instance",
		"instance_ref":"第1台",
		"gpu_pref":"5090",
		"zone_pref":"华北二A"
	}`)

	require.NoError(t, err)
	require.NotNil(t, decision)
	assert.Equal(t, ContextDecisionSelectEntity, decision.Decision)
	assert.Equal(t, ContextDecisionTargetInstance, decision.Target)
	assert.Equal(t, "第1台", decision.InstanceRef)
	assert.Empty(t, decision.GPUPref, "entity selection must not smuggle create-task updates")
	assert.Empty(t, decision.ZonePref)
}

func TestParseContextDecisionSlotUpdatesAreSanitizedAndMirrored(t *testing.T) {
	decision, err := parseContextDecision(`{
		"decision":"continue_task",
		"target":"create",
		"gpu_pref":"5090",
		"zone_pref":"华北二A",
		"slot_updates":{
			"target_size_gb":"200G",
			"zone_id":"5001",
			"az_group":"cn-wlcb",
			"password":"secret",
			"image_source":"community"
		}
	}`)

	require.NoError(t, err)
	require.NotNil(t, decision)
	assert.Equal(t, ContextDecisionContinueTask, decision.Decision)
	assert.Equal(t, "5090", decision.GPUPref)
	assert.Equal(t, "华北二A", decision.ZonePref)
	assert.Equal(t, "community", decision.ImageSource)
	assert.Equal(t, "5090", decision.SlotUpdates["gpu_type"])
	assert.Equal(t, "华北二A", decision.SlotUpdates["zone"])
	assert.Equal(t, "200G", decision.SlotUpdates["target_size_gb"])
	assert.Equal(t, "community", decision.SlotUpdates["image_source"])
	assert.NotContains(t, decision.SlotUpdates, "zone_id")
	assert.NotContains(t, decision.SlotUpdates, "az_group")
	assert.NotContains(t, decision.SlotUpdates, "password")
}

func TestContextDecisionToCreateContinuationUsesOnlyContinueTask(t *testing.T) {
	cont := contextDecisionToContinuation(ContextDecision{
		Decision:    ContextDecisionContinueTask,
		GPUPref:     "5090",
		ZonePref:    "华北二A",
		ImageSource: "community",
	})

	require.NotNil(t, cont)
	assert.Equal(t, ContextContinuationContinue, cont.Decision)
	assert.Equal(t, "5090", cont.GPUPref)
	assert.Equal(t, "华北二A", cont.ZonePref)
	assert.Equal(t, "community", cont.ImageSource)

	assert.Nil(t, contextDecisionToContinuation(ContextDecision{
		Decision:    ContextDecisionSelectEntity,
		Target:      ContextDecisionTargetInstance,
		InstanceRef: "第1台",
	}))
}

func TestBuildContextDecisionPromptHasImperativeSafetyRules(t *testing.T) {
	msgs := buildContextDecisionPrompt(ContextDecisionInput{
		UserText:     "那华北二A呢",
		RouterIntent: intent.IntentStockAvailability,
		ActiveTask: ContextFrame{
			Kind:  ContextFrameKindDeploy,
			GPU:   "4090",
			Zone:  "cn-bj2-03",
			Stage: "create_failed",
		},
	})

	systemPrompt := msgs[0].Content
	for _, want := range []string{
		"只判断是否沿用上下文",
		"slot_updates",
		"不要生成最终 API 参数",
		"写操作必须交给后端确认卡",
		"不确定时输出 clarify",
	} {
		if !strings.Contains(systemPrompt, want) {
			t.Fatalf("system prompt missing %q:\n%s", want, systemPrompt)
		}
	}
}
