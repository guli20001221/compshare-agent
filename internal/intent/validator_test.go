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
		IntentUnknown,
	}, AllIntents())
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
