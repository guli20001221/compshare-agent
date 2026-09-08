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
	resolver := New(catalog, TargetAdjudicatorFunc(func(SlotCandidate) TargetVerdict { return TargetAccept }), MachineTypeCatalog{})

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
	resolver := New(catalog, TargetAdjudicatorFunc(func(SlotCandidate) TargetVerdict { return TargetAccept }), MachineTypeCatalog{Names: []string{"4090"}, Available: true})

	resolved := resolver.Resolve(ActionProposal{Operation: "CreateInstanceWorkflow", Slots: []SlotCandidate{
		{Name: "GpuType", Value: "4090"},
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
	resolver := New(catalog, TargetAdjudicatorFunc(func(SlotCandidate) TargetVerdict { return TargetAccept }), MachineTypeCatalog{})

	resolved := resolver.Resolve(ActionProposal{Operation: "CreateInstanceWorkflow", Slots: []SlotCandidate{
		{Name: "Cpu", Value: "not-a-number"},
	}})

	require.NotEmpty(t, resolved.Rejected)
	require.False(t, resolved.ReadyForConfirmation)
	require.True(t, resolved.ReadyForIntake, "an invalid value on a collectable field opens the form to re-collect it")
	require.NotContains(t, resolved.Arguments, "Cpu", "the invalid value is discarded, never carried forward")
	require.Equal(t, []RejectedProblem{{Slot: "Cpu", Kind: RejectInvalidValue, Actor: RejectionActorModel}}, resolved.RejectedProblems)
}

func TestInvalidValueRecordsWhoCanCorrectIt(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(catalog, TargetAdjudicatorFunc(func(SlotCandidate) TargetVerdict { return TargetAccept }), MachineTypeCatalog{})

	resolved := resolver.Resolve(ActionProposal{Operation: "CreateInstanceWorkflow", Slots: []SlotCandidate{
		{Name: "Cpu", Value: "not-a-number"},
	}})

	require.Equal(t, []RejectedProblem{{
		Slot: "Cpu", Kind: RejectInvalidValue, Actor: RejectionActorModel,
	}}, resolved.RejectedProblems)
}

// A rejection that is NOT a form-correctable invalid value must still block the
// form. Each of these is a distinct non-correctable channel the lead named.
func TestResolveNonCorrectableRejectionBlocksIntake(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)

	t.Run("unknown field", func(t *testing.T) {
		r := New(catalog, TargetAdjudicatorFunc(func(SlotCandidate) TargetVerdict { return TargetAccept }), MachineTypeCatalog{})
		resolved := r.Resolve(ActionProposal{Operation: "CreateInstanceWorkflow", Slots: []SlotCandidate{
			{Name: "Bogus", Value: "x"},
		}})
		require.False(t, resolved.ReadyForIntake, "an unknown field is not a form input")
	})

	t.Run("dependency failure blocks the form", func(t *testing.T) {
		// Verified source, but no zone catalog attached → the SERVER could not
		// adjudicate the value (outage). Never the user's fault to re-pick.
		r := New(catalog, TargetAdjudicatorFunc(func(SlotCandidate) TargetVerdict { return TargetAccept }), MachineTypeCatalog{})
		resolved := r.Resolve(ActionProposal{Operation: "CreateInstanceWorkflow", Slots: []SlotCandidate{
			{Name: "Zone", Value: "cn-wlcb-01"},
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
	require.Contains(t, spec.Fields, "Name")

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
		spec, err := intakeSpecForOperation(true, []string{"Zone"}, fields)
		require.NoError(t, err)
		require.Equal(t, IntakeGuided, spec.Mode)
	})
	t.Run("unknown field errors", func(t *testing.T) {
		_, err := intakeSpecForOperation(true, []string{"Nope"}, fields)
		require.Error(t, err)
	})
	t.Run("target field errors", func(t *testing.T) {
		_, err := intakeSpecForOperation(true, []string{"UHostId"}, fields)
		require.Error(t, err)
	})
	t.Run("secret field errors", func(t *testing.T) {
		_, err := intakeSpecForOperation(true, []string{"Password"}, fields)
		require.Error(t, err)
	})
	t.Run("guided with no fields errors", func(t *testing.T) {
		_, err := intakeSpecForOperation(true, nil, fields)
		require.Error(t, err)
	})
	t.Run("non-guided is inert", func(t *testing.T) {
		spec, err := intakeSpecForOperation(false, nil, fields)
		require.NoError(t, err)
		require.Equal(t, IntakeNone, spec.Mode)
	})
}

func TestStartModeIsAnAgentSemanticChoice(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(catalog, TargetAdjudicatorFunc(func(SlotCandidate) TargetVerdict { return TargetAccept }), MachineTypeCatalog{})

	ordinary := resolver.Resolve(ActionProposal{Operation: "StartInstanceWorkflow", Slots: []SlotCandidate{
		{Name: "StartMode", Value: "normal"},
	}})
	require.Equal(t, "normal", ordinary.Arguments["StartMode"])
	require.Empty(t, ordinary.Rejected)

	cpuOnly := resolver.Resolve(ActionProposal{Operation: "StartInstanceWorkflow", Slots: []SlotCandidate{
		{Name: "StartMode", Value: "cpu_only_8c16g"},
	}})
	require.Equal(t, "cpu_only_8c16g", cpuOnly.Arguments["StartMode"])
	require.Empty(t, cpuOnly.Rejected)
}

func TestValidOptionalCreateFieldsRemainInTheContract(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(catalog, TargetAdjudicatorFunc(func(SlotCandidate) TargetVerdict { return TargetAccept }),
		MachineTypeCatalog{Names: []string{"H20"}, Available: true})

	resolved := resolver.Resolve(ActionProposal{Operation: "CreateInstanceWorkflow", Slots: []SlotCandidate{
		{Name: "GpuType", Value: "H20"},
		{Name: "SystemDiskSize", Value: "190GB"},
		{Name: "Name", Value: "codex-e2e"},
	}})

	require.True(t, resolved.ReadyForConfirmation)
	require.Equal(t, float64(190), resolved.Arguments["SystemDiskSize"])
	require.Equal(t, "codex-e2e", resolved.Arguments["Name"])
}

func TestInvalidOptionalFieldWithoutAFormControlBlocksIntake(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(catalog, TargetAdjudicatorFunc(func(SlotCandidate) TargetVerdict { return TargetAccept }), MachineTypeCatalog{Names: []string{"4090"}, Available: true})

	resolved := resolver.Resolve(ActionProposal{Operation: "CreateInstanceWorkflow", Slots: []SlotCandidate{
		{Name: "GpuType", Value: "4090"},
		{Name: "SystemDiskSize", Value: "not-a-size"},
	}})

	require.Equal(t, []RejectedProblem{{Slot: "SystemDiskSize", Kind: RejectInvalidValue, Actor: RejectionActorModel}}, resolved.RejectedProblems)
	require.False(t, resolved.ReadyForConfirmation, "a rejected value never confirms straight through")
	require.False(t, resolved.ReadyForIntake, "the form cannot recollect an invalid disk size")
	require.NotContains(t, resolved.Arguments, "SystemDiskSize", "the bad value is dropped, never carried into the create")
}

func TestInvalidExactImageIDCannotBeDiscardedIntoAnUnrelatedPicker(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(
		catalog,
		TargetAdjudicatorFunc(func(SlotCandidate) TargetVerdict { return TargetAccept }),
		MachineTypeCatalog{Names: []string{"4090"}, Available: true},
	).WithImageCatalog(deployment.NewImageCatalogSnapshot(true, nil))

	resolved := resolver.Resolve(ActionProposal{Operation: "CreateInstanceWorkflow", Slots: []SlotCandidate{
		{Name: "GpuType", Value: "4090"},
		{Name: "CompShareImageId", Value: "img-stale"},
	}})

	require.Contains(t, resolved.RejectedProblems,
		RejectedProblem{Slot: "CompShareImageId", Kind: RejectInvalidValue, Actor: RejectionActorModel})
	require.False(t, resolved.ReadyForConfirmation)
	require.False(t, resolved.ReadyForIntake,
		"精确 ID 无效时必须阻断，不能静默丢掉后让卡片换成另一个镜像")
	require.NotContains(t, resolved.Arguments, "CompShareImageId")
}

func TestNonCollectableInvalidValueStillBlocksIntake(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(catalog, TargetAdjudicatorFunc(func(SlotCandidate) TargetVerdict { return TargetAccept }), MachineTypeCatalog{Names: []string{"4090"}, Available: true})

	resolved := resolver.Resolve(ActionProposal{Operation: "ResetPasswordWorkflow", Slots: []SlotCandidate{
		{Name: "UHostId", Value: "uhost-1"},
		{Name: "Password", Value: 12345},
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
	resolver := New(catalog, TargetAdjudicatorFunc(func(SlotCandidate) TargetVerdict { return TargetAccept }), MachineTypeCatalog{})

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
		RejectTargetNotExist:    "target_not_exist",
		RejectOperationContract: "operation_contract",
	}
	for k, want := range cases {
		require.Equal(t, want, k.String())
	}
	require.Equal(t, "unknown", RejectionKind(999).String())
}
