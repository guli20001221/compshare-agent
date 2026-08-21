package actionresolver

import (
	"testing"

	"github.com/compshare-agent/internal/workflow"
	"github.com/stretchr/testify/require"
)

func TestCatalogDeclaresEveryWorkflowFromAuthoritativeRegistries(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	require.Equal(t, workflow.RegisteredWorkflowActions(), catalog.Operations())
	for _, operation := range catalog.Operations() {
		spec, ok := catalog.Lookup(operation)
		require.True(t, ok, operation)
		require.NotEmpty(t, spec.Fields, operation)
		require.NotEmpty(t, spec.Execution, operation)
		require.True(t, spec.NeedsConfirm, operation)
		for name, field := range spec.Fields {
			require.Equal(t, name, field.Name)
			require.NotEmpty(t, field.Codec)
		}
	}
}

func TestCatalogDescriptionContainsOnlyItsCapabilityBoundary(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	for _, operation := range catalog.Operations() {
		spec, ok := catalog.Lookup(operation)
		require.True(t, ok, operation)
		require.Contains(t, spec.AgentDescription, "调用/边界：", operation)
		for _, repeated := range []string{"接续：", "失败：", "输入示例"} {
			require.NotContains(t, spec.AgentDescription, repeated, operation)
		}
	}
}

func TestResolverRequiresServerVerifiedTargetProvenance(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	verified := New(catalog, EvidenceVerifierFunc(func(candidate SlotCandidate) bool {
		return candidate.Evidence != nil && candidate.Evidence.Quote == "uhost-1"
	}), MachineTypeCatalog{})
	proposal := ActionProposal{TurnID: "turn-1", Operation: "StopInstanceWorkflow", Slots: []SlotCandidate{{
		Name: "UHostId", Value: "uhost-1", Source: SourceUserExplicit,
		Evidence: &SourceEvidence{MessageID: "turn-1", Start: 3, End: 10, Quote: "uhost-1"},
	}}}
	resolved := verified.Resolve(proposal)
	require.True(t, resolved.ReadyForConfirmation)
	require.Equal(t, "uhost-1", resolved.Arguments["UHostId"])
	require.Equal(t, SourceUserExplicit, resolved.Provenance["UHostId"].Source)
	require.NotNil(t, resolved.Confirmation)
	require.Equal(t, "SafeToolExecutor", resolved.Gate.Executor)

	unverified := New(catalog, nil, MachineTypeCatalog{}).Resolve(proposal)
	require.False(t, unverified.ReadyForConfirmation)
	require.NotEmpty(t, unverified.Rejected)

	proposal.Slots[0].Source = SourceAgentInference
	inferred := verified.Resolve(proposal)
	require.False(t, inferred.ReadyForConfirmation)
	require.NotEmpty(t, inferred.Rejected)
}

func TestResolverNeverSilentlyChoosesConflictingCurrentValues(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(catalog, EvidenceVerifierFunc(func(SlotCandidate) bool { return true }), MachineTypeCatalog{})
	resolved := resolver.Resolve(ActionProposal{Operation: "CreateDiskWorkflow", Slots: []SlotCandidate{
		{Name: "UHostId", Value: "uhost-8g", Source: SourceVerifiedContext, Evidence: &SourceEvidence{ContextField: "selected_entities"}},
		{Name: "Size", Value: 30, Source: SourceUserExplicit, Evidence: &SourceEvidence{Quote: "30"}},
		{Name: "Size", Value: 50, Source: SourceUserExplicit, Evidence: &SourceEvidence{Quote: "50"}},
	}})
	require.False(t, resolved.ReadyForConfirmation)
	require.Len(t, resolved.Conflicts, 1)
	require.Equal(t, "Size", resolved.Conflicts[0].Slot)
	require.Equal(t, "uhost-8g", resolved.Arguments["UHostId"])
	require.NotEqual(t, "8", resolved.Arguments["Size"], "resource id text must never become capacity")
}

func TestResolverDoesNotTrustInstanceNamesWithoutServerEvidence(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(catalog, EvidenceVerifierFunc(func(SlotCandidate) bool { return false }), MachineTypeCatalog{})
	for _, value := range []string{"test", "host", "a"} {
		resolved := resolver.Resolve(ActionProposal{Operation: "StopInstanceWorkflow", Slots: []SlotCandidate{{
			Name: "UHostId", Value: value, Source: SourceUserExplicit, Evidence: &SourceEvidence{Quote: value},
		}}})
		require.False(t, resolved.ReadyForConfirmation, value)
		require.NotEmpty(t, resolved.Rejected, value)
	}
}

func TestOperationSpecificStructuredValidatorRejectsAmbiguity(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(catalog, EvidenceVerifierFunc(func(SlotCandidate) bool { return true }), MachineTypeCatalog{})
	resolved := resolver.Resolve(ActionProposal{Operation: "SetStopSchedulerWorkflow", Slots: []SlotCandidate{
		{Name: "UHostId", Value: "uhost-1", Source: SourceToolObservation, Evidence: &SourceEvidence{ContextField: "recent_observations"}},
		{Name: "Schedule", Value: map[string]any{
			"mode": "today", "local_time": "23:00", "minutes": float64(30),
		}, Source: SourceAgentInference},
	}})
	require.False(t, resolved.ReadyForConfirmation)
	require.Contains(t, resolved.Rejected, "mode=today 不允许 minutes 或 at")
}

func TestCatalogMarksSensitiveAndResourceFields(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	spec, ok := catalog.Lookup("ResetPasswordWorkflow")
	require.True(t, ok)
	require.Equal(t, CodecResourceRef, spec.Fields["UHostId"].Codec)
	require.Equal(t, CodecSensitiveText, spec.Fields["Password"].Codec)
}

func TestCorrectionOverridesInheritedValueButTwoCorrectionsConflict(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(catalog, EvidenceVerifierFunc(func(SlotCandidate) bool { return true }), MachineTypeCatalog{})
	base := []SlotCandidate{{Name: "UHostId", Value: "uhost-1", Source: SourceVerifiedContext, Evidence: &SourceEvidence{ContextField: "selected_entities"}}}
	resolved := resolver.Resolve(ActionProposal{Operation: "RenameInstanceWorkflow", Slots: append(base,
		SlotCandidate{Name: "Name", Value: "old", Source: SourceVerifiedContext},
		SlotCandidate{Name: "Name", Value: "new", Source: SourceUserExplicit, Evidence: &SourceEvidence{Quote: "new"}},
	)})
	require.True(t, resolved.ReadyForConfirmation)
	require.Equal(t, "new", resolved.Arguments["Name"])

	conflicted := resolver.Resolve(ActionProposal{Operation: "RenameInstanceWorkflow", Slots: append(base,
		SlotCandidate{Name: "Name", Value: "new-a", Source: SourceUserExplicit, Evidence: &SourceEvidence{Quote: "new-a"}},
		SlotCandidate{Name: "Name", Value: "new-b", Source: SourceUserExplicit, Evidence: &SourceEvidence{Quote: "new-b"}},
	)})
	require.False(t, conflicted.ReadyForConfirmation)
	require.Len(t, conflicted.Conflicts, 1)
}

func TestConfirmationPreviewRedactsSensitiveValues(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(catalog, EvidenceVerifierFunc(func(SlotCandidate) bool { return true }), MachineTypeCatalog{})
	resolved := resolver.Resolve(ActionProposal{Operation: "ResetPasswordWorkflow", Slots: []SlotCandidate{
		{Name: "UHostId", Value: "uhost-1", Source: SourceToolObservation, Evidence: &SourceEvidence{ContextField: "recent_observations"}},
		{Name: "Password", Value: "SecurePass123!", Source: SourceUserExplicit, Evidence: &SourceEvidence{Quote: "SecurePass123!"}},
	}})
	require.True(t, resolved.ReadyForConfirmation)
	require.Equal(t, "[REDACTED]", resolved.Confirmation.Arguments["Password"])
	require.Equal(t, "SecurePass123!", resolved.Arguments["Password"])
}
