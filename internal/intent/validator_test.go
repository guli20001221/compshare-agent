package intent

import (
	"testing"

	"github.com/compshare-agent/internal/entity"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidatePlan_AcceptsValidMonitorPlan(t *testing.T) {
	reg := testRegistry(t)
	plan := validMonitorPlan()

	err := ValidatePlan(plan, ValidationContext{
		UserText: "看看 uhost-abc123 的 CPU 和 GPU 监控",
		Registry: reg,
	})

	require.NoError(t, err)
}

func TestValidatePlan_RejectsInvalidSchemaVersion(t *testing.T) {
	plan := validMonitorPlan()
	plan.SchemaVersion = "2.0"

	err := ValidatePlan(plan, ValidationContext{UserText: "看看 uhost-abc123 的监控", Registry: testRegistry(t)})

	requireValidationCode(t, err, ErrInvalidSchemaVersion)
}

func TestValidatePlan_RejectsInvalidIntentEnum(t *testing.T) {
	plan := validMonitorPlan()
	plan.Intent = Intent("made_up_intent")

	err := ValidatePlan(plan, ValidationContext{UserText: "看看 uhost-abc123 的监控", Registry: testRegistry(t)})

	requireValidationCode(t, err, ErrInvalidIntent)
}

func TestValidatePlan_RejectsLegacyMixedIntentEnums(t *testing.T) {
	for _, legacy := range []Intent{IntentMixedDiagnosisKB, IntentMixedBillingKB} {
		t.Run(string(legacy), func(t *testing.T) {
			plan := validMonitorPlan()
			plan.Intent = legacy

			err := ValidatePlan(plan, ValidationContext{UserText: "monitor uhost-abc123", Registry: testRegistry(t)})

			requireValidationCode(t, err, ErrInvalidIntent)
		})
	}
}

func TestValidatePlan_RejectsInvalidSlotType(t *testing.T) {
	plan := validMonitorPlan()
	plan.Slots.TargetRefs[0].Type = TargetRefType("uhost_id_planner_generated")

	err := ValidatePlan(plan, ValidationContext{UserText: "看看 uhost-abc123 的监控", Registry: testRegistry(t)})

	requireValidationCode(t, err, ErrInvalidTargetRefType)
}

func TestValidatePlan_RejectsMissingOrMismatchedProvenance(t *testing.T) {
	plan := validMonitorPlan()
	plan.Slots.TargetRefs[0].SourceSpan = "uhost-not-in-user-text"

	err := ValidatePlan(plan, ValidationContext{UserText: "看看 uhost-abc123 的监控", Registry: testRegistry(t)})

	requireValidationCode(t, err, ErrAttemptedHallucinatedEntity)
}

func TestValidatePlan_RejectsInvalidRequiredTool(t *testing.T) {
	plan := validMonitorPlan()
	plan.RequiredTools = []string{"DeleteEverything"}

	err := ValidatePlan(plan, ValidationContext{UserText: "看看 uhost-abc123 的监控", Registry: testRegistry(t)})

	requireValidationCode(t, err, ErrInvalidRequiredTool)
}

func TestValidatePlan_AcceptsStockCapacityPrecheckTool(t *testing.T) {
	plan := Plan{
		SchemaVersion: SchemaVersion,
		Intent:        IntentStockAvailability,
		RequiredTools: []string{"DescribeAvailableCompShareInstanceTypes", "CheckCompShareResourceCapacity"},
		Retrieval:     Retrieval{Enabled: false},
		Confidence:    0.8,
	}

	err := ValidatePlan(plan, ValidationContext{UserText: "4090 现在有没有货", Registry: testRegistry(t)})

	require.NoError(t, err)
}

func TestValidatePlan_CapabilityRegistryToolsStayAllowed(t *testing.T) {
	for _, entry := range capabilityRegistry {
		tools := []string{entry.requiredTool}
		tools = append(tools, extraHandlerActions()[entry.intent]...)

		plan := Plan{
			SchemaVersion: SchemaVersion,
			Intent:        entry.intent,
			RequiredTools: tools,
			Retrieval:     Retrieval{Enabled: false},
			Confidence:    0.8,
		}

		err := ValidatePlan(plan, ValidationContext{UserText: "capability query", Registry: testRegistry(t)})

		require.NoError(t, err, "capability intent %q tools %v must stay allowed", entry.intent, tools)
	}
}

func TestValidatePlan_RejectsRequiredToolOutsideIntentAllowlist(t *testing.T) {
	tests := []struct {
		name string
		plan Plan
	}{
		{
			name: "knowledge qa cannot declare api tools",
			plan: Plan{
				SchemaVersion: SchemaVersion,
				Intent:        IntentKnowledgeQA,
				RequiredTools: []string{"DescribeCompShareInstance"},
				Retrieval:     Retrieval{Enabled: false},
				Confidence:    0.8,
			},
		},
		{
			name: "resource info cannot declare monitor tools",
			plan: Plan{
				SchemaVersion: SchemaVersion,
				Intent:        IntentResourceInfo,
				RequiredTools: []string{"GetCompShareInstanceMonitor"},
				Retrieval:     Retrieval{Enabled: false},
				Confidence:    0.8,
			},
		},
		{
			name: "capability cannot declare another capability tool",
			plan: Plan{
				SchemaVersion: SchemaVersion,
				Intent:        IntentPlatformImageList,
				RequiredTools: []string{"DescribeCompShareCustomImages"},
				Retrieval:     Retrieval{Enabled: false},
				Confidence:    0.8,
			},
		},
		{
			name: "unknown cannot declare tools",
			plan: Plan{
				SchemaVersion: SchemaVersion,
				Intent:        IntentUnknown,
				RequiredTools: []string{"GetCompShareInstancePrice"},
				Retrieval:     Retrieval{Enabled: false},
				Confidence:    0.8,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePlan(tt.plan, ValidationContext{UserText: "test", Registry: testRegistry(t)})

			requireValidationCode(t, err, ErrInvalidRequiredTool)
		})
	}
}

func TestValidatePlan_AcceptsRequiredToolsForIntentAllowlist(t *testing.T) {
	tests := []Plan{
		{
			SchemaVersion: SchemaVersion,
			Intent:        IntentResourceInfo,
			RequiredTools: []string{"DescribeCompShareInstance"},
			Retrieval:     Retrieval{Enabled: false},
			Confidence:    0.8,
		},
		{
			SchemaVersion: SchemaVersion,
			Intent:        IntentMonitorQuery,
			RequiredTools: []string{"DescribeCompShareInstance", "GetCompShareInstanceMonitor"},
			Retrieval:     Retrieval{Enabled: false},
			Confidence:    0.8,
		},
		{
			SchemaVersion: SchemaVersion,
			Intent:        IntentBillingInstance,
			RequiredTools: []string{"DescribeCompShareInstance", "DiagnoseBilling"},
			Retrieval:     Retrieval{Enabled: false},
			Confidence:    0.8,
		},
		{
			SchemaVersion: SchemaVersion,
			Intent:        IntentKnowledgeQA,
			RequiredTools: []string{},
			Retrieval:     Retrieval{Enabled: false},
			Confidence:    0.8,
		},
	}

	for _, plan := range tests {
		t.Run(string(plan.Intent), func(t *testing.T) {
			err := ValidatePlan(plan, ValidationContext{UserText: "test", Registry: testRegistry(t)})

			require.NoError(t, err)
		})
	}
}

func TestValidatePlan_RejectsInvalidMetricEnum(t *testing.T) {
	plan := validMonitorPlan()
	plan.Slots.Metrics = []Metric{MetricCPU, Metric("disk")}

	err := ValidatePlan(plan, ValidationContext{UserText: "看看 uhost-abc123 的监控", Registry: testRegistry(t)})

	requireValidationCode(t, err, ErrInvalidMetric)
}

func TestValidatePlan_RejectsInvalidTimeWindowType(t *testing.T) {
	plan := validMonitorPlan()
	plan.Slots.TimeWindow.Type = TimeWindowType("made_up")

	err := ValidatePlan(plan, ValidationContext{UserText: "看看 uhost-abc123 的监控", Registry: testRegistry(t)})

	requireValidationCode(t, err, ErrInvalidTimeWindow)
}

func TestValidatePlan_RejectsAccountUnsupportedWithTargetRefs(t *testing.T) {
	plan := validMonitorPlan()
	plan.Intent = IntentBillingAccountUnsupported
	plan.RequiredTools = nil

	err := ValidatePlan(plan, ValidationContext{UserText: "查一下账号余额和 uhost-abc123", Registry: testRegistry(t)})

	requireValidationCode(t, err, ErrInvalidTargetRefType)
}

func TestValidatePlan_AcceptsResourceFilterSlots(t *testing.T) {
	plan := Plan{
		SchemaVersion: SchemaVersion,
		Intent:        IntentResourceInfo,
		Slots: Slots{TargetRefs: []TargetRef{
			{Type: TargetRefFilter, Value: "state=running"},
			{Type: TargetRefFilter, Value: "gpu_type=4090"},
		}},
		RequiredTools: []string{"DescribeCompShareInstance"},
		Retrieval:     Retrieval{Enabled: false},
		Confidence:    0.8,
	}

	err := ValidatePlan(plan, ValidationContext{UserText: "running 4090 instances", Registry: testRegistry(t)})

	require.NoError(t, err)
}

func TestValidatePlan_RejectsInvalidResourceFilterSlot(t *testing.T) {
	plan := Plan{
		SchemaVersion: SchemaVersion,
		Intent:        IntentResourceInfo,
		Slots:         Slots{TargetRefs: []TargetRef{{Type: TargetRefFilter, Value: "state=deleted"}}},
		RequiredTools: []string{"DescribeCompShareInstance"},
		Retrieval:     Retrieval{Enabled: false},
		Confidence:    0.8,
	}

	err := ValidatePlan(plan, ValidationContext{UserText: "deleted instances", Registry: testRegistry(t)})

	requireValidationCode(t, err, ErrInvalidTargetRefType)
}

func TestValidatePlan_EntityValidatorAcceptsUserProvidedIDWithMatchingSpan(t *testing.T) {
	plan := Plan{
		SchemaVersion: SchemaVersion,
		Intent:        IntentMonitorQuery,
		Slots: Slots{
			TargetRefs: []TargetRef{{
				Type:       TargetRefUHostIDUserInput,
				Value:      "uhost-abc123",
				Source:     SourceUserText,
				SourceSpan: "uhost-abc123",
			}},
		},
		RequiredTools: []string{"DescribeCompShareInstance", "GetCompShareInstanceMonitor"},
		Retrieval:     Retrieval{Enabled: false},
		Confidence:    0.8,
	}

	err := ValidatePlan(plan, ValidationContext{UserText: "帮我看 uhost-abc123", Registry: testRegistry(t)})

	require.NoError(t, err)
}

func TestValidatePlan_EntityValidatorRejectsUserProvidedIDWithoutMatchingSpan(t *testing.T) {
	plan := validMonitorPlan()
	plan.Slots.TargetRefs[0].SourceSpan = "这不是用户原文"

	err := ValidatePlan(plan, ValidationContext{UserText: "帮我看 uhost-abc123", Registry: testRegistry(t)})

	requireValidationCode(t, err, ErrAttemptedHallucinatedEntity)
}

func TestValidatePlan_RejectsShortNameSlot(t *testing.T) {
	plan := validMonitorPlan()
	plan.Slots.TargetRefs = []TargetRef{{
		Type:       TargetRefName,
		Value:      "a",
		Source:     SourceUserText,
		SourceSpan: "a",
	}}

	err := ValidatePlan(plan, ValidationContext{UserText: "看 a 这台", Registry: testRegistry(t)})

	requireValidationCode(t, err, ErrNameTooShort)
}

func TestIntentEnumDeclaresAllV1Intents(t *testing.T) {
	assert.ElementsMatch(t, []Intent{
		IntentMonitorQuery,
		IntentMonitorHistory,
		IntentResourceInfo,
		IntentBillingInstance,
		IntentBillingAccountUnsupported,
		IntentExpiryRenewal,
		IntentDiagnosis,
		IntentVagueFailure,
		IntentOperationLifecycle,
		IntentRecommendation,
		IntentKnowledgeQA,
		IntentMixedDiagnosisKB,
		IntentMixedBillingKB,
		// Capability Registry v1 (PR A, 2026-05-18) — see capability_registry.go.
		IntentGPUSpecsQuery,
		IntentStockAvailability,
		IntentPlatformImageList,
		IntentCustomImageList,
		IntentCommunityImageList,
		// PR #3 (2026-05-22) — pricing capability (commercial path).
		IntentPricingQuery,
		// disk_info (2026-05-29) — disk-listing routing; reuses
		// DescribeCompShareInstance.DiskSet since upstream has no list API.
		IntentDiskInfo,
		IntentUnknown,
	}, AllIntents())
}

func TestRuntimeIntentsExcludeLegacyMixedIntents(t *testing.T) {
	assert.NotContains(t, RuntimeIntents(), IntentMixedDiagnosisKB)
	assert.NotContains(t, RuntimeIntents(), IntentMixedBillingKB)
}

func validMonitorPlan() Plan {
	return Plan{
		SchemaVersion: SchemaVersion,
		Intent:        IntentMonitorQuery,
		Scope:         "single_instance",
		Slots: Slots{
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
		},
		RequiredTools: []string{"DescribeCompShareInstance", "GetCompShareInstanceMonitor"},
		Retrieval:     Retrieval{Enabled: false},
		Confidence:    0.92,
		Reasoning:     "monitor query",
	}
}

func testRegistry(t *testing.T) *entity.EntityRegistry {
	t.Helper()
	reg := entity.NewRegistry()
	require.NoError(t, reg.SyncFromDescribe(map[string]any{
		"TotalCount": float64(1),
		"UHostSet": []any{
			map[string]any{
				"UHostId": "uhost-abc123",
				"Name":    "train-a",
				"State":   "Running",
				"GpuType": "4090",
				"GPU":     float64(1),
			},
		},
	}, "test"))
	return reg
}

func requireValidationCode(t *testing.T, err error, code ErrorCode) {
	t.Helper()
	require.Error(t, err)
	var validationErr *ValidationError
	require.ErrorAs(t, err, &validationErr)
	assert.Equal(t, code, validationErr.Code)
}
