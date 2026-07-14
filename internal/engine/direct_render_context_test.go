package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/intent"
	grounded "github.com/compshare-agent/internal/renderer"
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

	eng := &Engine{}
	for _, value := range intents {
		t.Run(string(value), func(t *testing.T) {
			spec := eng.directRenderTaskSpec(intent.IntentRoute{Intent: value}, "那它呢？")
			assert.Equal(t, string(value), spec.Intent)
			assert.Equal(t, "那它呢？", spec.CurrentQuestion)
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
