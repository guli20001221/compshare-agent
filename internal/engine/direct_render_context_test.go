package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	grounded "github.com/compshare-agent/internal/renderer"
	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDirectRenderTaskSpecCoversProductionDirectIntentContract(t *testing.T) {
	intents := []intent.Intent{
		intent.IntentResourceInfo,
		intent.IntentMonitorQuery,
		intent.IntentGPUSpecsQuery,
		intent.IntentStockAvailability,
		intent.IntentPricingQuery,
		intent.IntentRefundEstimate,
		intent.IntentImageTagCatalog,
		intent.IntentModelRepositoryBrowse,
		intent.IntentImageList,
		intent.IntentNetAcceleratorStatus,
	}
	require.Len(t, contextAwareDirectIntents, len(intents))

	eng := &Engine{
		sessionStateHydrated: true,
		sessionState:         SessionState{},
		messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleUser, Content: "上一轮用户问题"},
			{Role: openai.ChatMessageRoleAssistant, Content: "上一轮完整回答"},
			{Role: openai.ChatMessageRoleUser, Content: "那它呢？"},
		},
	}
	for _, value := range intents {
		t.Run(string(value), func(t *testing.T) {
			spec := eng.directRenderTaskSpec(intent.IntentRoute{Intent: value}, "那它呢？")
			assert.Equal(t, string(value), spec.Intent)
			assert.Equal(t, "那它呢？", spec.CurrentQuestion)
			assert.Contains(t, spec.ContextSummary, "上一轮用户问题")
			assert.Contains(t, spec.ContextSummary, "上一轮完整回答")
		})
	}

	for _, value := range []intent.Intent{
		intent.IntentKnowledgeQA,
		intent.IntentUnknown,
		intent.IntentBillingAccountUnsupported,
		intent.IntentOperationLifecycle,
	} {
		t.Run("excluded_"+string(value), func(t *testing.T) {
			assert.Empty(t, eng.directRenderTaskSpec(intent.IntentRoute{Intent: value}, "那它呢？"))
		})
	}
}

func TestDirectRenderTaskSpecCarriesRecentCompleteTurnButNotCurrentPendingTurn(t *testing.T) {
	// Recent in-memory history is available even for embedders/CLI paths that do
	// not hydrate durable SessionState.
	eng := &Engine{}
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleUser, Content: "4090 卡显存多少？"},
		{Role: openai.ChatMessageRoleAssistant, Content: "4090 标准版显存为 24 GB。"},
		{Role: openai.ChatMessageRoleUser, Content: "那它多少钱？"},
	}

	spec := eng.directRenderTaskSpec(intent.IntentRoute{Intent: intent.IntentPricingQuery}, "那它多少钱？")

	assert.Equal(t, "那它多少钱？", spec.CurrentQuestion)
	assert.Contains(t, spec.ContextSummary, "4090 卡显存多少")
	assert.Contains(t, spec.ContextSummary, "4090 标准版显存为 24 GB")
	assert.NotContains(t, spec.ContextSummary, "那它多少钱")
}

func TestDirectClarificationFromRealPricingFollowupContinuesWithContextAwareAgent(t *testing.T) {
	// Sanitized from the production session whose sequence ended with:
	// "假如不开机，是不是不收费" -> a billing explanation -> "是多少钱？".
	// The old direct handler ignored that pair and asked for a GPU model again.
	previousAgentic := tools.AgenticSearchKnowledgeEnabled()
	tools.SetAgenticSearchKnowledgeEnabled(false)
	t.Cleanup(func() { tools.SetAgenticSearchKnowledgeEnabled(previousAgentic) })
	exec := &mockExecutor{results: map[string]map[string]any{
		"DescribeAvailableCompShareInstanceTypes": {
			"AvailableInstanceTypes": []any{map[string]any{
				"Name": "4090", "Collection": []any{map[string]any{"Zone": "cn-wlcb-01", "CPU": []any{float64(16)}, "Memory": []any{float64(64)}}},
			}},
		},
	}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "结合上一轮，您问的是关机后仍保留资源的费用；需要先区分云盘和镜像。"}}}
	eng := NewWithDeps(mock, exec, nil)
	eng.InitWithContext("test user")
	eng.messages = append(eng.messages,
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "假如不开机，是不是不收费"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "按量关机后 CPU、GPU 和内存停止收费，但云盘和镜像仍可能收费。"},
	)
	eng.SetIntentPlanner(&scriptedIntentPlanner{results: []intent.IntentRouterResult{{
		Plan: intent.IntentRoute{SchemaVersion: intent.SchemaVersion, Intent: intent.IntentPricingQuery, Confidence: 0.9},
	}}}, IntentPlannerOptions{EnabledIntents: []intent.Intent{intent.IntentPricingQuery}})

	reply, err := eng.Chat(context.Background(), "是多少钱？", noopStep)

	require.NoError(t, err)
	assert.Contains(t, reply, "关机后仍保留资源的费用")
	assert.NotContains(t, reply, "请告诉我您想查的 GPU 型号")
	require.Len(t, mock.calls, 1)
	var visible strings.Builder
	for _, message := range mock.calls[0].Messages {
		visible.WriteString(message.Content)
	}
	assert.Contains(t, visible.String(), "按量关机后 CPU、GPU 和内存停止收费")
	for _, tool := range mock.calls[0].Tools {
		if tool.Function != nil {
			assert.NotContains(t, strings.ToLower(tool.Function.Name), "workflow")
		}
	}
}

func TestContextDependentPlainDirectReplyContinuesInScopedAgent(t *testing.T) {
	eng := &Engine{messages: []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "有哪些共享镜像？"},
		{Role: openai.ChatMessageRoleAssistant, Content: "目前有镜像 A 和镜像 B。"},
		{Role: openai.ChatMessageRoleUser, Content: "还有呢？"},
	}}

	clarification := intent.HandledResult("请说明你想继续看哪类镜像。")
	clarification.NeedsClarification = true
	assert.True(t, eng.shouldResolveContextDependentDirectReplyInAgent(
		clarification, "还有呢？",
	), "an explicit clarification should let the context-aware agent resolve the referent")
	assert.False(t, eng.shouldResolveContextDependentDirectReplyInAgent(
		intent.HandledResult("未找到符合条件的退款记录。"), "还有呢？",
	), "authoritative deterministic replies must not be replaced merely because they have no envelope")
	failure := intent.HandlerResult{
		Status:             intent.HandlerStatusFailureAfterTool,
		Reply:              "查询失败",
		FailureClass:       intent.HandlerFailureGenericRead,
		NeedsClarification: true,
	}
	assert.False(t, eng.shouldResolveContextDependentDirectReplyInAgent(
		failure, "还有呢？",
	), "read failures have their own context-aware fallback and advisory")

	env := envelope.Envelope{Kind: envelope.KindResourceInfo}
	groundedResult := intent.HandledResult("deterministic")
	groundedResult.Envelope = &env
	assert.False(t, eng.shouldResolveContextDependentDirectReplyInAgent(
		groundedResult, "还有呢？",
	), "an envelope-backed result can consume context in the grounded renderer")
	assert.False(t, eng.shouldResolveContextDependentDirectReplyInAgent(
		intent.HandledResult("当前镜像列表"), "查看共享镜像",
	), "a self-contained current question does not need historical resolution")
}

func TestGroundedDirectRendererReceivesShortFollowupAsTaskNotEvidence(t *testing.T) {
	renderer := &mockGroundedGenerator{result: grounded.RenderResult{
		Text:            "train-a 当前状态是 Running。",
		AttributionMode: grounded.AttributionEnvelope,
	}}
	eng := &Engine{
		groundedRenderer:      renderer,
		groundedRendererModel: "test-model",
		sessionStateHydrated:  true,
		sessionState: SessionState{
			TaskSnapshot: TaskSnapshot{
				Goal:      "继续查看训练实例",
				Status:    TaskSnapshotStatusActive,
				Freshness: ContinuityFreshnessFresh,
			},
			ConversationDigest: ConversationDigest{
				Narrative: "此前在查看 train-a 的运行状态",
			},
		},
	}
	env := envelope.Envelope{
		Kind:          envelope.KindResourceInfo,
		SourceActions: []string{"DescribeCompShareInstance"},
		Subjects: []envelope.Subject{{
			ID: "uhost-a", Name: "train-a", Type: envelope.SubjectInstance,
		}},
		Facts: []envelope.Fact{{
			SubjectID: "uhost-a", Key: "state", Label: "状态", Value: "Running", Source: envelope.FactSourceAPI,
		}},
	}
	handled := intent.HandlerResult{Reply: "fallback", Envelope: &env}

	reply := eng.renderGroundedHandlerResult(
		context.Background(),
		handled,
		intent.IntentRoute{Intent: intent.IntentResourceInfo},
		"那它现在呢？",
	)

	assert.Equal(t, "train-a 当前状态是 Running。", reply)
	require.Len(t, renderer.requests, 1)
	request := renderer.requests[0]
	assert.Equal(t, "那它现在呢？", request.TaskSpec.CurrentQuestion)
	assert.Equal(t, "resource_info", request.TaskSpec.Intent)
	assert.Equal(t, "继续查看训练实例", request.TaskSpec.Goal)
	assert.Equal(t, "此前在查看 train-a 的运行状态", request.TaskSpec.ContextSummary)
	payload, err := json.Marshal(request.Envelope)
	require.NoError(t, err)
	assert.NotContains(t, string(payload), "那它现在呢")
}

func TestPlainDeterministicFollowupGetsContextEnvelope(t *testing.T) {
	eng := &Engine{messages: []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "有哪些共享镜像？"},
		{Role: openai.ChatMessageRoleAssistant, Content: "有镜像 A 和镜像 B。"},
		{Role: openai.ChatMessageRoleUser, Content: "还有呢？"},
	}}
	result := intent.HandledResult("本轮查询返回镜像 A、镜像 B、镜像 C。")
	result.ToolAction = "DescribeCompShareSharingImages"

	got := eng.contextEnvelopeForPlainDirectReply(result, intent.IntentRoute{Intent: intent.IntentImageList}, "还有呢？")

	require.NotNil(t, got.Envelope)
	assert.Equal(t, envelope.KindContextualDirectReply, got.Envelope.Kind)
	require.Len(t, got.Envelope.Facts, 1)
	assert.Equal(t, result.Reply, got.Envelope.Facts[0].Value)
	assert.NotContains(t, got.Envelope.Facts[0].Value, "有哪些共享镜像",
		"conversation is understanding context, never evidence")
}

func TestGroundedDirectRendererBudgetReturnsDeterministicAnswer(t *testing.T) {
	renderer := &mockGroundedGenerator{result: grounded.RenderResult{Text: "不应调用"}}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.groundedRenderer = renderer
	eng.groundedRendererModel = "test-model"
	eng.maxTokensPerTurn = 10
	eng.turnTokensConsumed = 10
	env := envelope.Envelope{Kind: envelope.KindResourceInfo}

	got := eng.renderGroundedHandlerResult(context.Background(), intent.HandlerResult{
		Status: intent.HandlerStatusHandled, Reply: "train-a 当前正在运行。", Envelope: &env,
	}, intent.IntentRoute{Intent: intent.IntentResourceInfo}, "它现在呢")

	assert.Equal(t, "train-a 当前正在运行。", got)
	assert.Empty(t, renderer.requests, "budget blocks only the optional renderer call")
}

func TestDirectRenderTaskSpecCarriesSemanticMemoryButNoExecutionAuthority(t *testing.T) {
	eng := &Engine{
		sessionStateHydrated: true,
		sessionState: SessionState{
			SelectedInstanceID:        "uhost-a",
			SelectedInstanceName:      "train-a",
			SelectedInstanceSource:    SelectedInstanceSourceObserved,
			SelectedInstanceFreshness: ContinuityFreshnessStale,
			ContextFrame: ContextFrame{
				Slots: map[string]string{
					"instance_id": "uhost-a",
					"password":    "must-not-cross-render-boundary",
				},
				SlotSources: map[string]string{"instance_id": "model"},
			},
			RecentFacts: []ToolFact{{
				Kind:      FactKindPriceQuote,
				SubjectID: "uhost-a",
				Payload:   map[string]any{"private_value": "must-not-cross-render-boundary"},
			}},
			TaskSnapshot: TaskSnapshot{
				Goal:         "查看训练实例当前负载",
				Stage:        "waiting_for_metric",
				Constraints:  []string{"只看 GPU"},
				Decisions:    []string{"目标是 train-a"},
				MissingSlots: []string{"time_window"},
				Status:       TaskSnapshotStatusActive,
				Freshness:    ContinuityFreshnessStale,
				Entities: []SemanticEntityHint{{
					Kind: "instance", ID: "uhost-a", Name: "train-a", Source: "model", Freshness: ContinuityFreshnessStale,
				}},
			},
			ConversationDigest: ConversationDigest{
				Narrative:       "用户此前在查看训练实例的负载",
				Constraints:     []string{"不要切换实例"},
				Decisions:       []string{"继续查看同一实例"},
				UnresolvedTasks: []string{"确认当前 GPU 负载"},
				EntityHints: []SemanticEntityHint{{
					Kind: "instance", ID: "uhost-a", Name: "train-a", Source: "observed", Freshness: ContinuityFreshnessStale,
				}},
			},
		},
	}

	spec := eng.directRenderTaskSpec(intent.IntentRoute{Intent: intent.IntentMonitorQuery}, "现在呢？")
	assert.Equal(t, "现在呢？", spec.CurrentQuestion)
	assert.Equal(t, "查看训练实例当前负载", spec.Goal)
	assert.Equal(t, "用户此前在查看训练实例的负载", spec.ContextSummary)
	assert.Contains(t, spec.Constraints, "只看 GPU")
	assert.Contains(t, spec.Constraints, "不要切换实例")
	assert.Contains(t, spec.UnresolvedTasks, "确认当前 GPU 负载")
	require.Len(t, spec.EntityHints, 1)
	assert.Equal(t, "uhost-a", spec.EntityHints[0].ID)
	assert.Equal(t, "train-a", spec.EntityHints[0].Name)
	assert.Equal(t, ContinuityFreshnessStale, spec.EntityHints[0].Freshness)

	payload, err := json.Marshal(spec)
	require.NoError(t, err)
	assert.NotContains(t, string(payload), "must-not-cross-render-boundary")
	assert.NotContains(t, string(payload), `"source"`)
	assert.NotContains(t, string(payload), `"password"`)
}

func TestDirectRenderTaskSpecDoesNotReviveResolvedTask(t *testing.T) {
	eng := &Engine{
		sessionStateHydrated: true,
		sessionState: SessionState{
			TaskSnapshot: TaskSnapshot{
				Goal:      "已经结束的扩容任务",
				Status:    TaskSnapshotStatusResolved,
				Decisions: []string{"扩到 200G"},
			},
			ConversationDigest: ConversationDigest{
				Narrative: "早期讨论过扩容",
			},
		},
	}

	spec := eng.directRenderTaskSpec(intent.IntentRoute{Intent: intent.IntentResourceInfo}, "我有几台？")
	assert.Empty(t, spec.Goal)
	assert.Empty(t, spec.Stage)
	assert.Empty(t, spec.MissingSlots)
	assert.Equal(t, "早期讨论过扩容", spec.ContextSummary)
}
