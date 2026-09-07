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

func TestResolverRequiresExactTargetVerification(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	verified := New(catalog, TargetAdjudicatorFunc(func(candidate SlotCandidate) TargetVerdict {
		if candidate.Value == "uhost-1" {
			return TargetAccept
		}
		return TargetReject
	}), MachineTypeCatalog{})
	proposal := ActionProposal{TurnID: "turn-1", Operation: "StopInstanceWorkflow", Slots: []SlotCandidate{{
		Name: "UHostId", Value: "uhost-1",
	}}}
	resolved := verified.Resolve(proposal)
	require.True(t, resolved.ReadyForConfirmation)
	require.Equal(t, "uhost-1", resolved.Arguments["UHostId"])

	require.NotNil(t, resolved.Confirmation)
	require.Equal(t, "SafeToolExecutor", resolved.Gate.Executor)

	unverified := New(catalog, nil, MachineTypeCatalog{}).Resolve(proposal)
	require.False(t, unverified.ReadyForConfirmation)
	require.NotEmpty(t, unverified.Rejected)

}

func TestResolverNeverSilentlyChoosesConflictingCurrentValues(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(catalog, TargetAdjudicatorFunc(func(SlotCandidate) TargetVerdict { return TargetAccept }), MachineTypeCatalog{})
	resolved := resolver.Resolve(ActionProposal{Operation: "CreateDiskWorkflow", Slots: []SlotCandidate{
		{Name: "UHostId", Value: "uhost-8g"},
		{Name: "Size", Value: 30},
		{Name: "Size", Value: 50},
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
	resolver := New(catalog, TargetAdjudicatorFunc(func(SlotCandidate) TargetVerdict { return TargetReject }), MachineTypeCatalog{})
	for _, value := range []string{"test", "host", "a"} {
		resolved := resolver.Resolve(ActionProposal{Operation: "StopInstanceWorkflow", Slots: []SlotCandidate{{
			Name: "UHostId", Value: value,
		}}})
		require.False(t, resolved.ReadyForConfirmation, value)
		require.NotEmpty(t, resolved.Rejected, value)
	}
}

func TestOperationSpecificStructuredValidatorUsesScheduleModeAsDiscriminator(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	spec, ok := catalog.Lookup("SetStopSchedulerWorkflow")
	require.True(t, ok)
	require.NoError(t, spec.ValidateResolved(map[string]any{
		"Schedule": map[string]any{
			// Tomorrow keeps this structural discriminator test independent of the
			// wall clock while still exercising the local_time branch.
			"mode": "tomorrow", "local_time": "23:00", "minutes": float64(30),
		},
	}))
}

func TestCatalogMarksSensitiveAndResourceFields(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	spec, ok := catalog.Lookup("ResetPasswordWorkflow")
	require.True(t, ok)
	require.Equal(t, CodecResourceRef, spec.Fields["UHostId"].Codec)
	require.Equal(t, CodecSensitiveText, spec.Fields["Password"].Codec)
}

func TestConfirmationPreviewRedactsSensitiveValues(t *testing.T) {
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	resolver := New(catalog, TargetAdjudicatorFunc(func(SlotCandidate) TargetVerdict { return TargetAccept }), MachineTypeCatalog{})
	resolved := resolver.Resolve(ActionProposal{Operation: "ResetPasswordWorkflow", Slots: []SlotCandidate{
		{Name: "UHostId", Value: "uhost-1"},
		{Name: "Password", Value: "SecurePass123!"},
	}})
	require.True(t, resolved.ReadyForConfirmation)
	require.Equal(t, "[REDACTED]", resolved.Confirmation.Arguments["Password"])
	require.Equal(t, "SecurePass123!", resolved.Arguments["Password"])
}
