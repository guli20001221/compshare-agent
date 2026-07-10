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

	err := ValidateRoute(plan, ValidationContext{
		UserText: "看看 uhost-abc123 的 CPU 和 GPU 监控",
		Registry: reg,
	})

	require.NoError(t, err)
}

func TestValidatePlan_RejectsInvalidSchemaVersion(t *testing.T) {
	plan := validMonitorPlan()
	plan.SchemaVersion = "2.0"

	err := ValidateRoute(plan, ValidationContext{UserText: "看看 uhost-abc123 的监控", Registry: testRegistry(t)})

	requireValidationCode(t, err, ErrInvalidSchemaVersion)
}

func TestValidatePlan_RejectsInvalidIntentEnum(t *testing.T) {
	plan := validMonitorPlan()
	plan.Intent = Intent("made_up_intent")

	err := ValidateRoute(plan, ValidationContext{UserText: "看看 uhost-abc123 的监控", Registry: testRegistry(t)})

	requireValidationCode(t, err, ErrInvalidIntent)
}

func TestValidatePlan_ValidatesLifecycleActionEnum(t *testing.T) {
	validActions := []LifecycleAction{
		"",
		LifecycleActionStop,
		LifecycleActionStart,
		LifecycleActionReboot,
		LifecycleActionReinstall,
		LifecycleActionResize,
		LifecycleActionResetPwd,
		LifecycleActionRename,
		LifecycleActionCreateDisk,
	}
	for _, action := range validActions {
		t.Run("valid_"+string(action), func(t *testing.T) {
			plan := IntentRoute{
				SchemaVersion: SchemaVersion,
				Intent:        IntentOperationLifecycle,
				Slots:         Slots{Action: action},
				Confidence:    0.9,
			}
			require.NoError(t, ValidateRoute(plan, ValidationContext{}))
		})
	}

	plan := IntentRoute{
		SchemaVersion: SchemaVersion,
		Intent:        IntentOperationLifecycle,
		Slots:         Slots{Action: LifecycleAction("destroy")},
		Confidence:    0.9,
	}
	err := ValidateRoute(plan, ValidationContext{})
	requireValidationCode(t, err, ErrorCode("invalid_lifecycle_action"))
	var validationErr *ValidationError
	require.ErrorAs(t, err, &validationErr)
	assert.Equal(t, "slots.action", validationErr.Field)
}

func TestValidatePlan_RejectsRemovedIntentEnums(t *testing.T) {
	for _, legacy := range []Intent{
		Intent("recommendation"),
		Intent("mixed_diagnosis_kb"),
		Intent("mixed_billing_kb"),
	} {
		t.Run(string(legacy), func(t *testing.T) {
			plan := validMonitorPlan()
			plan.Intent = legacy

			err := ValidateRoute(plan, ValidationContext{UserText: "monitor uhost-abc123", Registry: testRegistry(t)})

			requireValidationCode(t, err, ErrInvalidIntent)
		})
	}
}

func TestValidatePlan_RejectsInvalidSlotType(t *testing.T) {
	plan := validMonitorPlan()
	plan.Slots.TargetRefs[0].Type = TargetRefType("uhost_id_planner_generated")

	err := ValidateRoute(plan, ValidationContext{UserText: "看看 uhost-abc123 的监控", Registry: testRegistry(t)})

	requireValidationCode(t, err, ErrInvalidTargetRefType)
}

func TestValidatePlan_RejectsMissingOrMismatchedProvenance(t *testing.T) {
	plan := validMonitorPlan()
	plan.Slots.TargetRefs[0].SourceSpan = "uhost-not-in-user-text"

	err := ValidateRoute(plan, ValidationContext{UserText: "看看 uhost-abc123 的监控", Registry: testRegistry(t)})

	requireValidationCode(t, err, ErrAttemptedHallucinatedEntity)
}

func TestValidatePlan_IgnoresDeprecatedPlannerControlFields(t *testing.T) {
	plan := validMonitorPlan()
	plan.RequiredTools = []string{"DeleteEverything"}
	plan.Retrieval = Retrieval{Enabled: true}
	plan.HardBlockHint = true

	err := ValidateRoute(plan, ValidationContext{UserText: "看看 uhost-abc123 的监控", Registry: testRegistry(t)})

	require.NoError(t, err)
}

func TestValidatePlan_AcceptsImageListSourceOnlyForImageList(t *testing.T) {
	valid := IntentRoute{
		SchemaVersion: SchemaVersion,
		Intent:        IntentImageList,
		Slots:         Slots{ImageSource: ImageSourceCustom},
		RequiredTools: []string{"DescribeCompShareCustomImages"},
		Retrieval:     Retrieval{Enabled: false},
		Confidence:    0.8,
	}
	require.NoError(t, ValidateRoute(valid, ValidationContext{UserText: "我自己保存的镜像有哪些", Registry: testRegistry(t)}))

	invalidSource := valid
	invalidSource.Slots.ImageSource = ImageSource("private")
	requireValidationCode(t, ValidateRoute(invalidSource, ValidationContext{UserText: "镜像", Registry: testRegistry(t)}), ErrInvalidImageSource)

	wrongIntent := valid
	wrongIntent.Intent = IntentKnowledgeQA
	wrongIntent.RequiredTools = nil
	requireValidationCode(t, ValidateRoute(wrongIntent, ValidationContext{UserText: "镜像区别", Registry: testRegistry(t)}), ErrInvalidImageSource)
}

func TestValidatePlan_AcceptsReadOnlyRefinementSlotsOnlyForTheirRoutes(t *testing.T) {
	valid := IntentRoute{
		SchemaVersion: SchemaVersion,
		Intent:        IntentCFSInfo,
		Slots: Slots{
			CFSKind:    CFSKindCreatePrice,
			SizeGB:     50,
			Zone:       "cn-bj2-03",
			ChargeType: "Year",
		},
		Confidence: 0.9,
	}
	require.NoError(t, ValidateRoute(valid, ValidationContext{UserText: "查 CFS 创建价格", Registry: testRegistry(t)}))

	wrongIntent := valid
	wrongIntent.Intent = IntentOperationLifecycle
	requireValidationCode(t, ValidateRoute(wrongIntent, ValidationContext{UserText: "创建 CFS", Registry: testRegistry(t)}), ErrInvalidReadOnlySlot)

	badPrice := IntentRoute{
		SchemaVersion: SchemaVersion,
		Intent:        IntentPricingQuery,
		Slots:         Slots{PriceKind: PriceKind("retail")},
		Confidence:    0.9,
	}
	requireValidationCode(t, ValidateRoute(badPrice, ValidationContext{UserText: "4090 价格", Registry: testRegistry(t)}), ErrInvalidReadOnlySlot)
}

func TestValidatePlan_RejectsInvalidMetricEnum(t *testing.T) {
	plan := validMonitorPlan()
	plan.Slots.Metrics = []Metric{MetricCPU, Metric("disk")}

	err := ValidateRoute(plan, ValidationContext{UserText: "看看 uhost-abc123 的监控", Registry: testRegistry(t)})

	requireValidationCode(t, err, ErrInvalidMetric)
}

func TestValidatePlan_RejectsInvalidTimeWindowType(t *testing.T) {
	plan := validMonitorPlan()
	plan.Slots.TimeWindow.Type = TimeWindowType("made_up")

	err := ValidateRoute(plan, ValidationContext{UserText: "看看 uhost-abc123 的监控", Registry: testRegistry(t)})

	requireValidationCode(t, err, ErrInvalidTimeWindow)
}

func TestValidatePlan_RejectsAccountUnsupportedWithTargetRefs(t *testing.T) {
	plan := validMonitorPlan()
	plan.Intent = IntentBillingAccountUnsupported
	plan.RequiredTools = nil

	err := ValidateRoute(plan, ValidationContext{UserText: "查一下账号余额和 uhost-abc123", Registry: testRegistry(t)})

	requireValidationCode(t, err, ErrInvalidTargetRefType)
}

func TestValidatePlan_AcceptsResourceFilterSlots(t *testing.T) {
	plan := IntentRoute{
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

	err := ValidateRoute(plan, ValidationContext{UserText: "running 4090 instances", Registry: testRegistry(t)})

	require.NoError(t, err)
}

func TestValidatePlan_RejectsInvalidResourceFilterSlot(t *testing.T) {
	plan := IntentRoute{
		SchemaVersion: SchemaVersion,
		Intent:        IntentResourceInfo,
		Slots:         Slots{TargetRefs: []TargetRef{{Type: TargetRefFilter, Value: "state=deleted"}}},
		RequiredTools: []string{"DescribeCompShareInstance"},
		Retrieval:     Retrieval{Enabled: false},
		Confidence:    0.8,
	}

	err := ValidateRoute(plan, ValidationContext{UserText: "deleted instances", Registry: testRegistry(t)})

	requireValidationCode(t, err, ErrInvalidTargetRefType)
}

func TestValidatePlan_EntityValidatorAcceptsUserProvidedIDWithMatchingSpan(t *testing.T) {
	plan := IntentRoute{
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

	err := ValidateRoute(plan, ValidationContext{UserText: "帮我看 uhost-abc123", Registry: testRegistry(t)})

	require.NoError(t, err)
}

func TestValidatePlan_EntityValidatorRejectsUserProvidedIDWithoutMatchingSpan(t *testing.T) {
	plan := validMonitorPlan()
	plan.Slots.TargetRefs[0].SourceSpan = "这不是用户原文"

	err := ValidateRoute(plan, ValidationContext{UserText: "帮我看 uhost-abc123", Registry: testRegistry(t)})

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

	err := ValidateRoute(plan, ValidationContext{UserText: "看 a 这台", Registry: testRegistry(t)})

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
		IntentKnowledgeQA,
		// Route Registry v1 (PR A, 2026-05-18) — see route_registry.go.
		IntentGPUSpecsQuery,
		IntentStockAvailability,
		IntentNetAcceleratorStatus,
		IntentRefundEstimate,
		IntentCFSInfo,
		IntentImageTagCatalog,
		IntentModelRepositoryBrowse,
		IntentImageList,
		// PR #3 (2026-05-22) — pricing route (commercial path).
		IntentPricingQuery,
		// disk_info (2026-05-29) — disk-listing routing; reuses
		// DescribeCompShareInstance.DiskSet since upstream has no list API.
		IntentDiskInfo,
		// deploy_model (B8.3, 2026-05-31) — agent-tier create skill via tryDeployModel.
		IntentDeployModel,
		IntentCreateInstance,
		IntentUnknown,
	}, AllIntents())
}

func TestRuntimeIntentsExcludeRemovedIntents(t *testing.T) {
	runtime := RuntimeIntents()
	assert.NotContains(t, runtime, Intent("recommendation"))
	assert.NotContains(t, runtime, Intent("mixed_diagnosis_kb"))
	assert.NotContains(t, runtime, Intent("mixed_billing_kb"))
}

func TestRuntimeIntentsIncludeCreateInstance(t *testing.T) {
	assert.Contains(t, RuntimeIntents(), IntentCreateInstance)
	assert.Contains(t, AllIntents(), IntentCreateInstance)
}

func validMonitorPlan() IntentRoute {
	return IntentRoute{
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
