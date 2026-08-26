package actionresolver

import (
	"testing"

	"github.com/compshare-agent/internal/deployment"
	"github.com/stretchr/testify/require"
)

// A create proposal missing only a guided-collectable field is ReadyForIntake
// (open the form), NOT ReadyForConfirmation (execute) — the two are mutually
// exclusive. Intake carries no confirmation preview; the form collects first.
func TestResolveMarksIncompleteCreateReadyForIntake(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(catalog, EvidenceVerifierFunc(func(SlotCandidate) bool { return true }), MachineTypeCatalog{})

	resolved := resolver.Resolve(ActionProposal{Operation: "CreateInstanceWorkflow"})

	require.False(t, resolved.ReadyForConfirmation, "GpuType is still missing")
	require.True(t, resolved.ReadyForIntake, "a create missing only a guided-collectable field opens the intake form")
	require.Equal(t, []string{"GpuType"}, resolved.Missing)
	require.Nil(t, resolved.Confirmation, "intake is pre-confirmation")
}

// A complete create is confirm-ready and therefore NOT intake — the states never
// overlap.
func TestResolveCompleteCreateIsConfirmationNotIntake(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(catalog, EvidenceVerifierFunc(func(SlotCandidate) bool { return true }), MachineTypeCatalog{Names: []string{"4090"}, Available: true})

	resolved := resolver.Resolve(ActionProposal{Operation: "CreateInstanceWorkflow", Slots: []SlotCandidate{
		{Name: "GpuType", Value: "4090", Source: SourceUserExplicit, Evidence: &SourceEvidence{Quote: "4090"}},
	}})

	require.True(t, resolved.ReadyForConfirmation)
	require.False(t, resolved.ReadyForIntake, "a complete create confirms; it does not re-enter intake")
}

// An INVALID VALUE on a declared collectable field is form-correctable: the
// resolver drops the bad value (never silently swaps it) and the guided form
// re-collects a valid one. So it opens intake, not prose. (This refines the old
// blanket "any rejection blocks intake": the field IS one the form can fix.)
func TestResolveCorrectableInvalidValueOpensIntake(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(catalog, EvidenceVerifierFunc(func(SlotCandidate) bool { return true }), MachineTypeCatalog{})

	resolved := resolver.Resolve(ActionProposal{Operation: "CreateInstanceWorkflow", Slots: []SlotCandidate{
		{Name: "Cpu", Value: "not-a-number", Source: SourceUserExplicit, Evidence: &SourceEvidence{Quote: "not-a-number"}},
	}})

	require.NotEmpty(t, resolved.Rejected)
	require.False(t, resolved.ReadyForConfirmation)
	require.True(t, resolved.ReadyForIntake, "an invalid value on a collectable field opens the form to re-collect it")
	require.NotContains(t, resolved.Arguments, "Cpu", "the invalid value is discarded, never carried forward")
	require.Equal(t, []RejectedProblem{{Slot: "Cpu", Kind: RejectInvalidValue}}, resolved.RejectedProblems)
}

// A rejection that is NOT a form-correctable invalid value must still block the
// form. Each of these is a distinct non-correctable channel the lead named.
func TestResolveNonCorrectableRejectionBlocksIntake(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)

	t.Run("unknown field", func(t *testing.T) {
		r := New(catalog, EvidenceVerifierFunc(func(SlotCandidate) bool { return true }), MachineTypeCatalog{})
		resolved := r.Resolve(ActionProposal{Operation: "CreateInstanceWorkflow", Slots: []SlotCandidate{
			{Name: "Bogus", Value: "x", Source: SourceUserExplicit, Evidence: &SourceEvidence{Quote: "x"}},
		}})
		require.False(t, resolved.ReadyForIntake, "an unknown field is not a form input")
	})

	t.Run("unverified source on a collectable field", func(t *testing.T) {
		// Zone IS collectable, but a failed span verification is a trust-boundary
		// failure — never a "let the user re-pick" case, even for a form field.
		r := New(catalog, EvidenceVerifierFunc(func(SlotCandidate) bool { return false }), MachineTypeCatalog{})
		resolved := r.Resolve(ActionProposal{Operation: "CreateInstanceWorkflow", Slots: []SlotCandidate{
			{Name: "Zone", Value: "cn-wlcb-01", Source: SourceUserExplicit, Evidence: &SourceEvidence{Quote: "cn-wlcb-01"}},
		}})
		require.False(t, resolved.ReadyForIntake, "an unverified source blocks the form")
	})

	t.Run("dependency failure blocks the form", func(t *testing.T) {
		// Verified source, but no zone catalog attached → the SERVER could not
		// adjudicate the value (outage). Never the user's fault to re-pick.
		r := New(catalog, EvidenceVerifierFunc(func(SlotCandidate) bool { return true }), MachineTypeCatalog{})
		resolved := r.Resolve(ActionProposal{Operation: "CreateInstanceWorkflow", Slots: []SlotCandidate{
			{Name: "Zone", Value: "cn-wlcb-01", Source: SourceUserExplicit, Evidence: &SourceEvidence{Quote: "cn-wlcb-01"}},
		}})
		require.NotEmpty(t, resolved.DependencyFailures)
		require.False(t, resolved.ReadyForIntake, "a dependency failure blocks the form")
	})
}

// The create collectable set is the EXPLICIT declaration (the guided form's
// fields), not an auto-derivation over the schema.
func TestCreateCollectableFieldsAreDeclaredNotDerived(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	spec, ok := catalog.Lookup("CreateInstanceWorkflow")
	require.True(t, ok)
	require.Equal(t, IntakeGuided, spec.Intake.Mode)
	require.ElementsMatch(t,
		[]string{"GpuType", "Zone", "Gpu", "Cpu", "Memory", "ImageSource", "ImageName", "ChargeType"},
		spec.Intake.CollectableFields)
	require.NotContains(t, spec.Fields, "Name", "free-form model output must not name a newly created instance")
}

// intakeSpecForOperation rejects a misdeclared collectable set at build time — a
// typo or a non-form field would silently disable correction otherwise.
func TestIntakeSpecForOperationValidatesDeclaration(t *testing.T) {
	fields := map[string]FieldSpec{
		"Zone":     {Name: "Zone", Codec: CodecZone},
		"Name":     {Name: "Name", Codec: CodecConstrainedText},
		"GpuType":  {Name: "GpuType", Codec: CodecMachineType, Required: true},
		"UHostId":  {Name: "UHostId", Codec: CodecResourceRef, Target: true},
		"Password": {Name: "Password", Codec: CodecSensitiveText},
	}
	t.Run("valid", func(t *testing.T) {
		spec, err := intakeSpecForOperation(true, []string{"Zone"}, nil, fields)
		require.NoError(t, err)
		require.Equal(t, IntakeGuided, spec.Mode)
	})
	t.Run("unknown field errors", func(t *testing.T) {
		_, err := intakeSpecForOperation(true, []string{"Nope"}, nil, fields)
		require.Error(t, err)
	})
	t.Run("target field errors", func(t *testing.T) {
		_, err := intakeSpecForOperation(true, []string{"UHostId"}, nil, fields)
		require.Error(t, err)
	})
	t.Run("secret field errors", func(t *testing.T) {
		_, err := intakeSpecForOperation(true, []string{"Password"}, nil, fields)
		require.Error(t, err)
	})
	t.Run("guided with no fields errors", func(t *testing.T) {
		_, err := intakeSpecForOperation(true, nil, nil, fields)
		require.Error(t, err)
	})
	t.Run("non-guided is inert", func(t *testing.T) {
		spec, err := intakeSpecForOperation(false, nil, nil, fields)
		require.NoError(t, err)
		require.Equal(t, IntakeNone, spec.Mode)
	})
}

// The user-supplied declaration is explicit: deriving it from all optional
// fields would silently omit meaningful Agent-assisted values elsewhere.
func TestIntakeSpecRejectsUnsafeUserSuppliedDeclarations(t *testing.T) {
	fields := map[string]FieldSpec{
		"Zone":     {Name: "Zone", Codec: CodecZone},
		"Name":     {Name: "Name", Codec: CodecConstrainedText},
		"GpuType":  {Name: "GpuType", Codec: CodecMachineType, Required: true},
		"UHostId":  {Name: "UHostId", Codec: CodecResourceRef, Target: true},
		"Password": {Name: "Password", Codec: CodecSensitiveText},
	}
	t.Run("valid optional field", func(t *testing.T) {
		spec, err := intakeSpecForOperation(true, []string{"Zone"}, []string{"Name"}, fields)
		require.NoError(t, err)
		require.Equal(t, []string{"Name"}, spec.UserSuppliedOptionalFields)
	})
	t.Run("unknown field errors", func(t *testing.T) {
		_, err := intakeSpecForOperation(true, []string{"Zone"}, []string{"Nope"}, fields)
		require.Error(t, err)
	})
	t.Run("required field errors", func(t *testing.T) {
		// A rejected slot is adjudicated, so it never lands in Missing — discarding
		// a required value would run the operation without it.
		_, err := intakeSpecForOperation(true, []string{"Zone"}, []string{"GpuType"}, fields)
		require.Error(t, err)
	})
	t.Run("target field errors", func(t *testing.T) {
		_, err := intakeSpecForOperation(true, []string{"Zone"}, []string{"UHostId"}, fields)
		require.Error(t, err)
	})
	t.Run("secret field errors", func(t *testing.T) {
		// The reinstall password is the field the derived rule would have silenced.
		_, err := intakeSpecForOperation(true, []string{"Zone"}, []string{"Password"}, fields)
		require.Error(t, err)
	})
	t.Run("user-supplied field without guided intake errors", func(t *testing.T) {
		_, err := intakeSpecForOperation(false, nil, []string{"Name"}, fields)
		require.Error(t, err)
	})
}

func TestWorkflowFieldSourcePoliciesAreDeclared(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	for _, operation := range catalog.Operations() {
		spec, ok := catalog.Lookup(operation)
		require.True(t, ok)
		if operation == "CreateInstanceWorkflow" {
			require.Equal(t, []string{"DataDiskSize", "SystemDiskSize"}, spec.Intake.UserSuppliedOptionalFields)
		} else {
			require.Empty(t, spec.Intake.UserSuppliedOptionalFields)
		}
		if field, exists := spec.Fields["WithoutGpuSpec"]; exists {
			require.Equal(t, operation == "StartInstanceWorkflow", field.CurrentUserEvidence)
		}
	}
}

func TestWithoutGpuStartKeepsOnlyCurrentUserEvidence(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(catalog, EvidenceVerifierFunc(func(SlotCandidate) bool { return true }), MachineTypeCatalog{})

	inferred := resolver.Resolve(ActionProposal{Operation: "StartInstanceWorkflow", Slots: []SlotCandidate{
		{Name: "WithoutGpuSpec", Value: "A", Source: SourceAgentInference},
	}})
	require.NotContains(t, inferred.Arguments, "WithoutGpuSpec")
	require.Empty(t, inferred.Rejected)

	carried := resolver.Resolve(ActionProposal{Operation: "StartInstanceWorkflow", Slots: []SlotCandidate{
		{Name: "WithoutGpuSpec", Value: "B", Source: SourceVerifiedContext},
	}})
	require.NotContains(t, carried.Arguments, "WithoutGpuSpec")
	require.Empty(t, carried.Rejected)

	explicit := resolver.Resolve(ActionProposal{Operation: "StartInstanceWorkflow", Slots: []SlotCandidate{
		{Name: "WithoutGpuSpec", Value: "A", Source: SourceUserExplicit, Evidence: &SourceEvidence{Quote: "A"}},
	}})
	require.Equal(t, "A", explicit.Arguments["WithoutGpuSpec"])
}

func TestUngroundedOptionalCreateFieldsDoNotBecomeContractValues(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(catalog, EvidenceVerifierFunc(func(SlotCandidate) bool { return true }),
		MachineTypeCatalog{Names: []string{"H20"}, Available: true})

	resolved := resolver.Resolve(ActionProposal{Operation: "CreateInstanceWorkflow", Slots: []SlotCandidate{
		{Name: "GpuType", Value: "H20", Source: SourceAgentInference},
		{Name: "SystemDiskSize", Value: float64(1), Source: SourceAgentInference},
	}})

	require.True(t, resolved.ReadyForConfirmation)
	require.Empty(t, resolved.Rejected)
	require.NotContains(t, resolved.Arguments, "SystemDiskSize")
	require.Equal(t, "H20", resolved.Arguments["GpuType"])
}

func TestGroundedOptionalCreateFieldsRemainInTheContract(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(catalog, EvidenceVerifierFunc(func(SlotCandidate) bool { return true }),
		MachineTypeCatalog{Names: []string{"H20"}, Available: true})

	resolved := resolver.Resolve(ActionProposal{Operation: "CreateInstanceWorkflow", Slots: []SlotCandidate{
		{Name: "GpuType", Value: "H20", Source: SourceAgentInference},
		{Name: "SystemDiskSize", Value: "190GB", Source: SourceUserExplicit, Evidence: &SourceEvidence{Quote: "190GB"}},
	}})

	require.True(t, resolved.ReadyForConfirmation)
	require.Equal(t, float64(190), resolved.Arguments["SystemDiskSize"])
}

func TestInvalidUserSuppliedOptionalFieldBlocksIntake(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(catalog, EvidenceVerifierFunc(func(SlotCandidate) bool { return true }), MachineTypeCatalog{Names: []string{"4090"}, Available: true})

	resolved := resolver.Resolve(ActionProposal{Operation: "CreateInstanceWorkflow", Slots: []SlotCandidate{
		{Name: "GpuType", Value: "4090", Source: SourceUserExplicit, Evidence: &SourceEvidence{Quote: "4090"}},
		{Name: "SystemDiskSize", Value: "not-a-size", Source: SourceUserExplicit, Evidence: &SourceEvidence{Quote: "not-a-size"}},
	}})

	require.Equal(t, []RejectedProblem{{Slot: "SystemDiskSize", Kind: RejectInvalidValue}}, resolved.RejectedProblems)
	require.False(t, resolved.ReadyForConfirmation, "a rejected value never confirms straight through")
	require.False(t, resolved.ReadyForIntake, "the form cannot recollect an invalid disk size")
	require.NotContains(t, resolved.Arguments, "SystemDiskSize", "the bad value is dropped, never carried into the create")
}

func TestInvalidExactImageIDCannotBeDiscardedIntoAnUnrelatedPicker(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(
		catalog,
		EvidenceVerifierFunc(func(SlotCandidate) bool { return true }),
		MachineTypeCatalog{Names: []string{"4090"}, Available: true},
	).WithImageCatalog(deployment.NewImageCatalogSnapshot(true, nil))

	resolved := resolver.Resolve(ActionProposal{Operation: "CreateInstanceWorkflow", Slots: []SlotCandidate{
		{Name: "GpuType", Value: "4090", Source: SourceAgentInference},
		{Name: "CompShareImageId", Value: "img-stale", Source: SourceAgentInference},
	}})

	require.Contains(t, resolved.RejectedProblems,
		RejectedProblem{Slot: "CompShareImageId", Kind: RejectInvalidValue})
	require.False(t, resolved.ReadyForConfirmation)
	require.False(t, resolved.ReadyForIntake,
		"精确 ID 无效时必须阻断，不能静默丢掉后让卡片换成另一个镜像")
	require.NotContains(t, resolved.Arguments, "CompShareImageId")
}

func TestNonCollectableInvalidValueStillBlocksIntake(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(catalog, EvidenceVerifierFunc(func(SlotCandidate) bool { return true }), MachineTypeCatalog{Names: []string{"4090"}, Available: true})

	resolved := resolver.Resolve(ActionProposal{Operation: "ResetPasswordWorkflow", Slots: []SlotCandidate{
		{Name: "UHostId", Value: "uhost-1", Source: SourceUserConfirmation},
		{Name: "Password", Value: 12345, Source: SourceAgentInference},
	}})

	require.False(t, resolved.ReadyForConfirmation)
	require.False(t, resolved.ReadyForIntake, "reset-password declares no guided form")
}

// Guided intake is a per-operation declaration. An operation that does not
// declare it (here StopInstanceWorkflow) is never ReadyForIntake, even when the
// only thing missing is a required field — a missing write TARGET must be asked
// for, never collected by a create-style form.
func TestResolveNonGuidedOperationIsNeverReadyForIntake(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(catalog, EvidenceVerifierFunc(func(SlotCandidate) bool { return true }), MachineTypeCatalog{})

	resolved := resolver.Resolve(ActionProposal{Operation: "StopInstanceWorkflow"})

	require.Equal(t, []string{"UHostId"}, resolved.Missing)
	require.False(t, resolved.ReadyForConfirmation)
	require.False(t, resolved.ReadyForIntake, "StopInstanceWorkflow declares no guided intake")
}

// TestRejectionKindString pins the value-free codes the disposition trace renders
// for each typed rejection kind. An unknown value degrades to "unknown" rather
// than an empty string (which would read as "no rejection" in a trace).
func TestRejectionKindString(t *testing.T) {
	cases := map[RejectionKind]string{
		RejectInvalidValue:      "invalid_value",
		RejectUnknownOperation:  "unknown_operation",
		RejectUnknownField:      "unknown_field",
		RejectUnknownSource:     "unknown_source",
		RejectUnverifiedSource:  "unverified_source",
		RejectTargetNotExist:    "target_not_exist",
		RejectOperationContract: "operation_contract",
	}
	for k, want := range cases {
		require.Equal(t, want, k.String())
	}
	require.Equal(t, "unknown", RejectionKind(999).String())
}
