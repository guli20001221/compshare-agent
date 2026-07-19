package actionresolver

import (
	"testing"

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

// Intake is only for a clean incomplete proposal. A rejected field means a value
// WAS supplied and could not be honoured — that is not something a whitelist form
// collects, so the proposal is neither confirm-ready nor intake-ready.
func TestResolveRejectedFieldBlocksReadyForIntake(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(catalog, EvidenceVerifierFunc(func(SlotCandidate) bool { return true }), MachineTypeCatalog{})

	resolved := resolver.Resolve(ActionProposal{Operation: "CreateInstanceWorkflow", Slots: []SlotCandidate{
		{Name: "Cpu", Value: "not-a-number", Source: SourceUserExplicit, Evidence: &SourceEvidence{Quote: "not-a-number"}},
	}})

	require.NotEmpty(t, resolved.Rejected)
	require.False(t, resolved.ReadyForConfirmation)
	require.False(t, resolved.ReadyForIntake, "a rejected field is not a clean incomplete proposal")
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
