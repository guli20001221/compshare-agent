package intent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompileMinimalPlanCore_AllRuntimeIntentsProduceValidPlan(t *testing.T) {
	for _, intentValue := range RuntimeIntents() {
		t.Run(string(intentValue), func(t *testing.T) {
			plan := compileMinimalPlanCore(minimalPlanCore{Intent: intentValue})

			require.NoError(t, ValidateRoute(plan, ValidationContext{}))
			assert.Equal(t, SchemaVersion, plan.SchemaVersion)
			assert.Equal(t, intentValue, plan.Intent)
			assert.False(t, plan.Retrieval.Enabled)
			assert.Equal(t, requiredToolsForIntentSorted(intentValue), plan.RequiredTools)
			assert.Equal(t, DeriveSelectedSkills(plan), plan.Skills)
			for _, tool := range plan.RequiredTools {
				assert.Truef(t, validRequiredToolForIntent(intentValue, tool), "tool %s must be valid for %s", tool, intentValue)
			}
		})
	}
}

func TestCompileMinimalPlanCore_PreservesStructuredSlots(t *testing.T) {
	core := minimalPlanCore{
		Intent: IntentMonitorQuery,
		TargetRefs: []TargetRef{{
			Type:       TargetRefUHostIDUserInput,
			Value:      "uhost-abc123",
			Source:     SourceUserText,
			SourceSpan: "uhost-abc123",
		}},
		Metrics: []Metric{MetricCPU, MetricGPU},
		TimeWindow: &TimeWindow{
			Type:  TimeWindowPreset,
			Value: "last_60s",
		},
	}

	plan := compileMinimalPlanCore(core)

	require.NoError(t, ValidateRoute(plan, ValidationContext{
		UserText: "看看 uhost-abc123 的 CPU 和 GPU 监控",
		Registry: testRegistry(t),
	}))
	require.Len(t, plan.Slots.TargetRefs, 1)
	assert.Equal(t, "uhost-abc123", plan.Slots.TargetRefs[0].Value)
	assert.Equal(t, []Metric{MetricCPU, MetricGPU}, plan.Slots.Metrics)
	require.NotNil(t, plan.Slots.TimeWindow)
	assert.Equal(t, TimeWindowPreset, plan.Slots.TimeWindow.Type)
	assert.Equal(t, "last_60s", plan.Slots.TimeWindow.Value)
}

func TestCompileMinimalPlanCore_DerivesLifecycleActionAndTools(t *testing.T) {
	plan := compileMinimalPlanCore(minimalPlanCore{
		Intent: IntentOperationLifecycle,
		Action: LifecycleActionReboot,
	})

	require.NoError(t, ValidateRoute(plan, ValidationContext{}))
	assert.Equal(t, LifecycleActionReboot, plan.Slots.Action)
	assert.Equal(t, []string{"DescribeCompShareInstance"}, plan.RequiredTools)
	assert.Empty(t, plan.Skills)
}

func TestCompileMinimalPlanCore_DerivesRouteSkillAndExtraTools(t *testing.T) {
	plan := compileMinimalPlanCore(minimalPlanCore{Intent: IntentStockAvailability})

	require.NoError(t, ValidateRoute(plan, ValidationContext{}))
	assert.Equal(t, []string{
		"CheckCompShareResourceCapacity",
		"DescribeAvailableCompShareInstanceTypes",
		"DescribeCompShareGpuInventory",
		"DescribeCompShareImages",
		"DescribeCompShareSupportZone",
	}, plan.RequiredTools)
	require.Len(t, plan.Skills, 1)
	assert.Equal(t, "stock_availability", plan.Skills[0].Name)
	assert.Equal(t, SkillResolutionDerivedFromIntent, plan.Skills[0].Resolution)
}

func TestCompileMinimalPlanCore_DoesNotAliasInputSlicesOrPointers(t *testing.T) {
	core := minimalPlanCore{
		Intent:     IntentMonitorQuery,
		TargetRefs: []TargetRef{{Type: TargetRefFilter, Value: "state=running"}},
		Metrics:    []Metric{MetricCPU},
		TimeWindow: &TimeWindow{Type: TimeWindowPreset, Value: "now"},
	}

	plan := compileMinimalPlanCore(core)
	core.TargetRefs[0].Value = "state=stopped"
	core.Metrics[0] = MetricGPU
	core.TimeWindow.Value = "last_60s"

	assert.Equal(t, "state=running", plan.Slots.TargetRefs[0].Value)
	assert.Equal(t, []Metric{MetricCPU}, plan.Slots.Metrics)
	require.NotNil(t, plan.Slots.TimeWindow)
	assert.Equal(t, "now", plan.Slots.TimeWindow.Value)
}
