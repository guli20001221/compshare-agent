package intent

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/compshare-agent/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectPlannerTrace_DisabledWritesEmptySlots(t *testing.T) {
	trace := ProjectPlannerTrace(IntentRouterResult{}, PlannerTraceOptions{Enabled: false})

	assert.False(t, trace.Enabled)
	assert.Empty(t, trace.Model)
	assert.Empty(t, trace.Intent)
	assert.Empty(t, trace.PlannedExecutionPath)
	assert.False(t, trace.SchemaValid)
	assert.NotNil(t, trace.Slots.TargetRefs)
	assert.Empty(t, trace.Slots.TargetRefs)
	assert.NotNil(t, trace.Slots.Metrics)
	assert.Empty(t, trace.Slots.Metrics)
	assert.Nil(t, trace.Slots.TimeWindow)
}

func TestProjectPlannerTrace_ValidMonitorPlan(t *testing.T) {
	trace := ProjectPlannerTrace(IntentRouterResult{
		Plan: IntentRoute{
			SchemaVersion: SchemaVersion,
			Intent:        IntentMonitorQuery,
			Slots: Slots{
				Metrics: []Metric{MetricGPU, MetricVRAM},
				TimeWindow: &TimeWindow{
					Type:  TimeWindowPreset,
					Value: "today",
				},
			},
			Confidence: 0.87,
		},
		Attempts: 1,
		Usage: llm.TokenUsage{
			PromptTokens:     31,
			CompletionTokens: 17,
			TotalTokens:      48,
		},
	}, PlannerTraceOptions{
		Enabled: true,
		Model:   "deepseek-v4-flash",
		Latency: 123 * time.Millisecond,
	})

	assert.True(t, trace.Enabled)
	assert.Equal(t, "deepseek-v4-flash", trace.Model)
	assert.Equal(t, int64(123), trace.LatencyMS)
	assert.Equal(t, 31, trace.InputTokens)
	assert.Equal(t, 17, trace.OutputTokens)
	assert.True(t, trace.SchemaValid)
	assert.Equal(t, string(IntentMonitorQuery), trace.Intent)
	assert.Equal(t, string(ExecutionPathRouting), trace.PlannedExecutionPath)
	assert.Equal(t, []string{"gpu", "vram"}, trace.Slots.Metrics)
	assert.InDelta(t, 0.87, trace.Confidence, 0.0001)

	window, ok := trace.Slots.TimeWindow.(PlannerTraceTimeWindow)
	require.True(t, ok)
	assert.Equal(t, string(TimeWindowPreset), window.Type)
	assert.Equal(t, "today", window.Value)
	assert.Empty(t, window.ValueHash)
}

func TestProjectPlannerTrace_ProjectsCodeDerivedSkills(t *testing.T) {
	trace := ProjectPlannerTrace(IntentRouterResult{
		Plan: IntentRoute{
			SchemaVersion: SchemaVersion,
			Intent:        IntentDeployModel,
			Slots:         Slots{},
			RequiredTools: []string{"DescribeCompShareImages"},
			Retrieval:     Retrieval{Enabled: false},
			Confidence:    0.82,
		},
	}, PlannerTraceOptions{Enabled: true, Model: "deepseek-v4-flash"})

	require.Len(t, trace.Skills, 1)
	assert.Equal(t, string(ExecutionPathAgent), trace.PlannedExecutionPath)
	assert.Equal(t, "deploy_model", trace.Skills[0].Name)
	assert.Equal(t, SkillResolutionAgentArm, trace.Skills[0].Resolution)
}

func TestProjectPlannerTrace_HashesTargetRefsAndNonAllowlistedTimeWindow(t *testing.T) {
	const rawID = "uhost-abc123"
	const rawName = "prod-gpu-01"
	const sourceSpan = "用户显式输入 uhost-abc123"
	const rawWindow = "2026-05-09T01:00:00+08:00/2026-05-09T02:00:00+08:00"
	const rawReasoning = "secret reasoning mentions uhost-abc123 and prod-gpu-01"

	trace := ProjectPlannerTrace(IntentRouterResult{
		Plan: IntentRoute{
			SchemaVersion: SchemaVersion,
			Intent:        IntentMonitorHistory,
			Slots: Slots{
				TargetRefs: []TargetRef{
					{Type: TargetRefUHostIDUserInput, Value: rawID, Source: SourceUserText, SourceSpan: sourceSpan},
					{Type: TargetRefName, Value: rawName, Source: SourcePriorTurn},
				},
				Metrics: []Metric{MetricGPU},
				TimeWindow: &TimeWindow{
					Type:  TimeWindowAbsolute,
					Value: rawWindow,
				},
			},
			Confidence: 0.7,
			Reasoning:  rawReasoning,
		},
	}, PlannerTraceOptions{Enabled: true, Model: "deepseek-v4-flash"})

	require.Len(t, trace.Slots.TargetRefs, 2)
	first, ok := trace.Slots.TargetRefs[0].(PlannerTraceTargetRef)
	require.True(t, ok)
	assert.Equal(t, string(TargetRefUHostIDUserInput), first.Type)
	assert.Equal(t, string(SourceUserText), first.Source)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, first.ValueHash)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, first.SourceSpanHash)

	second, ok := trace.Slots.TargetRefs[1].(PlannerTraceTargetRef)
	require.True(t, ok)
	assert.Equal(t, string(TargetRefName), second.Type)
	assert.Equal(t, string(SourcePriorTurn), second.Source)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, second.ValueHash)
	assert.Empty(t, second.SourceSpanHash)

	window, ok := trace.Slots.TimeWindow.(PlannerTraceTimeWindow)
	require.True(t, ok)
	assert.Equal(t, string(TimeWindowAbsolute), window.Type)
	assert.Empty(t, window.Value)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, window.ValueHash)

	data, err := json.Marshal(trace)
	require.NoError(t, err)
	raw := string(data)
	assert.NotContains(t, raw, rawID)
	assert.NotContains(t, raw, rawName)
	assert.NotContains(t, raw, sourceSpan)
	assert.NotContains(t, raw, rawWindow)
	assert.NotContains(t, raw, rawReasoning)
}

// The read-only slots added when the keyword sub-classifiers were retired (#423)
// ARE the router's decision variable, so a trace that omits them cannot explain
// why a turn was refined the way it was. They must be projected — but under the
// same two-class rule the TargetRef/TimeWindow projections already follow:
// validator-constrained enums verbatim, un-validated user text hashed.
func TestProjectPlannerTrace_ProjectsReadOnlySlots_EnumsVerbatimFreeTextHashed(t *testing.T) {
	const rawQuery = "数字人 uhost-abc123"
	const rawZone = "乌兰察布"

	trace := ProjectPlannerTrace(IntentRouterResult{
		Plan: IntentRoute{
			SchemaVersion: SchemaVersion,
			Intent:        IntentCFSInfo,
			Slots: Slots{
				ImageSource: ImageSourceCommunity,
				ListMode:    ListModeAll,
				PriceKind:   PriceKindCatalog,
				CFSKind:     CFSKindRefund,
				ChargeType:  "Spot",
				DetailLevel: DetailLevelFull,
				Action:      LifecycleActionStop,
				SizeGB:      1024,
				SearchQuery: rawQuery,
				Zone:        rawZone,
			},
		},
	}, PlannerTraceOptions{Enabled: true, Model: "deepseek-v4-flash"})

	// Closed-enum / bounded slots: verbatim, because they carry no user text.
	assert.Equal(t, string(ImageSourceCommunity), trace.Slots.ImageSource)
	assert.Equal(t, string(ListModeAll), trace.Slots.ListMode)
	assert.Equal(t, string(PriceKindCatalog), trace.Slots.PriceKind)
	assert.Equal(t, string(CFSKindRefund), trace.Slots.CFSKind)
	assert.Equal(t, "Spot", trace.Slots.ChargeType)
	assert.Equal(t, string(DetailLevelFull), trace.Slots.DetailLevel)
	assert.Equal(t, string(LifecycleActionStop), trace.Slots.Action)
	assert.Equal(t, 1024, trace.Slots.SizeGB)

	// Free-form user-derived slots: hashed, never raw.
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, trace.Slots.SearchQueryHash)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, trace.Slots.ZoneHash)

	data, err := json.Marshal(trace)
	require.NoError(t, err)
	raw := string(data)
	assert.NotContains(t, raw, rawQuery)
	assert.NotContains(t, raw, rawZone)
	// The instance id embedded in the query must not reach the trace either.
	assert.NotContains(t, raw, "uhost-abc123")
}

// Turns that set none of the new slots must serialize exactly as before, so
// existing traces and their consumers stay byte-stable.
func TestProjectPlannerTrace_UnsetReadOnlySlotsAreOmitted(t *testing.T) {
	trace := ProjectPlannerTrace(IntentRouterResult{
		Plan: IntentRoute{SchemaVersion: SchemaVersion, Intent: IntentResourceInfo},
	}, PlannerTraceOptions{Enabled: true, Model: "deepseek-v4-flash"})

	data, err := json.Marshal(trace)
	require.NoError(t, err)
	raw := string(data)
	for _, key := range []string{
		"image_source", "list_mode", "price_kind", "cfs_kind", "charge_type",
		"detail_level", "action", "size_gb", "search_query_hash", "zone_hash",
	} {
		assert.NotContains(t, raw, key, "unset slot %q must be omitted from the trace", key)
	}
}

func TestProjectPlannerTrace_DerivesHardBlockHintFromIntent(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan IntentRoute
		want bool
	}{
		{
			name: "billing_account_unsupported",
			plan: IntentRoute{SchemaVersion: SchemaVersion, Intent: IntentBillingAccountUnsupported, Confidence: 0.91},
			want: true,
		},
		{
			name: "model_supplied_hint_ignored",
			plan: IntentRoute{SchemaVersion: SchemaVersion, Intent: IntentKnowledgeQA, HardBlockHint: true, Confidence: 0.91},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := ProjectPlannerTrace(IntentRouterResult{
				Plan: tc.plan,
			}, PlannerTraceOptions{Enabled: true, Model: "deepseek-v4-flash"})

			assert.True(t, trace.Enabled)
			assert.True(t, trace.SchemaValid)
			assert.Equal(t, tc.want, trace.HardBlockHint)
		})
	}
}

func TestProjectPlannerTrace_StableHashesForEqualPlans(t *testing.T) {
	result := IntentRouterResult{
		Plan: IntentRoute{
			SchemaVersion: SchemaVersion,
			Intent:        IntentMonitorQuery,
			Slots: Slots{
				TargetRefs: []TargetRef{
					{Type: TargetRefName, Value: "prod-gpu-01", Source: SourcePriorTurn},
				},
				TimeWindow: &TimeWindow{Type: TimeWindowRelative, Value: "last_2h"},
			},
			Confidence: 0.6,
		},
	}

	first := ProjectPlannerTrace(result, PlannerTraceOptions{Enabled: true, Model: "deepseek-v4-flash"})
	second := ProjectPlannerTrace(result, PlannerTraceOptions{Enabled: true, Model: "deepseek-v4-flash"})

	require.Len(t, first.Slots.TargetRefs, 1)
	require.Len(t, second.Slots.TargetRefs, 1)
	firstRef := first.Slots.TargetRefs[0].(PlannerTraceTargetRef)
	secondRef := second.Slots.TargetRefs[0].(PlannerTraceTargetRef)
	assert.Equal(t, firstRef.ValueHash, secondRef.ValueHash)

	firstWindow := first.Slots.TimeWindow.(PlannerTraceTimeWindow)
	secondWindow := second.Slots.TimeWindow.(PlannerTraceTimeWindow)
	assert.Equal(t, firstWindow.ValueHash, secondWindow.ValueHash)
}
