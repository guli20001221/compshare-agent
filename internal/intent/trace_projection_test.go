package intent

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectPlannerTrace_DisabledWritesEmptySlots(t *testing.T) {
	trace := ProjectPlannerTrace(PlannerResult{}, PlannerTraceOptions{Enabled: false})

	assert.False(t, trace.Enabled)
	assert.Empty(t, trace.Model)
	assert.Empty(t, trace.Intent)
	assert.False(t, trace.SchemaValid)
	assert.NotNil(t, trace.Slots.TargetRefs)
	assert.Empty(t, trace.Slots.TargetRefs)
	assert.NotNil(t, trace.Slots.Metrics)
	assert.Empty(t, trace.Slots.Metrics)
	assert.Nil(t, trace.Slots.TimeWindow)
}

func TestProjectPlannerTrace_ValidMonitorPlan(t *testing.T) {
	trace := ProjectPlannerTrace(PlannerResult{
		Plan: Plan{
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
	}, PlannerTraceOptions{
		Enabled: true,
		Model:   "deepseek-v4-flash",
		Latency: 123 * time.Millisecond,
	})

	assert.True(t, trace.Enabled)
	assert.Equal(t, "deepseek-v4-flash", trace.Model)
	assert.Equal(t, int64(123), trace.LatencyMS)
	assert.True(t, trace.SchemaValid)
	assert.Equal(t, string(IntentMonitorQuery), trace.Intent)
	assert.Equal(t, []string{"gpu", "vram"}, trace.Slots.Metrics)
	assert.InDelta(t, 0.87, trace.Confidence, 0.0001)

	window, ok := trace.Slots.TimeWindow.(PlannerTraceTimeWindow)
	require.True(t, ok)
	assert.Equal(t, string(TimeWindowPreset), window.Type)
	assert.Equal(t, "today", window.Value)
	assert.Empty(t, window.ValueHash)
}

func TestProjectPlannerTrace_HashesTargetRefsAndNonAllowlistedTimeWindow(t *testing.T) {
	const rawID = "uhost-abc123"
	const rawName = "prod-gpu-01"
	const sourceSpan = "用户显式输入 uhost-abc123"
	const rawWindow = "2026-05-09T01:00:00+08:00/2026-05-09T02:00:00+08:00"

	trace := ProjectPlannerTrace(PlannerResult{
		Plan: Plan{
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
}

func TestProjectPlannerTrace_StableHashesForEqualPlans(t *testing.T) {
	result := PlannerResult{
		Plan: Plan{
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
