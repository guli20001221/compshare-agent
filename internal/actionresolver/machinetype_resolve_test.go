package actionresolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createProposal(gpuType string) ActionProposal {
	return ActionProposal{
		TurnID:    "turn-1",
		Operation: "CreateInstanceWorkflow",
		Slots: []SlotCandidate{{
			Name: "GpuType", Value: gpuType, Source: SourceUserExplicit,
			Evidence: &SourceEvidence{MessageID: "turn-1", Quote: gpuType},
		}},
	}
}

func createResolver(t *testing.T, machineTypes MachineTypeCatalog) *Resolver {
	t.Helper()
	catalog, err := BuildCatalog()
	require.NoError(t, err)
	return New(catalog, EvidenceVerifierFunc(func(SlotCandidate) bool { return true }), machineTypes)
}

// TestResolveCanonicalizesGpuTypeBeforeConfirmation is the contract the whole
// vertical exists for: the value the user CONFIRMS is the value that EXECUTES.
// Canonicalization happens before ReadyForConfirmation, so Arguments (what the
// workflow consumes) and Confirmation.Arguments (what the card shows) carry the
// identical canonical string. The old design normalized in executeWorkflow —
// AFTER the card was rendered and the contract sealed — so the two could differ
// and neither layer owned the final say.
func TestResolveCanonicalizesGpuTypeBeforeConfirmation(t *testing.T) {
	resolver := createResolver(t, liveCatalog())

	resolved := resolver.Resolve(createProposal("4090 48G"))

	require.True(t, resolved.ReadyForConfirmation, resolved.Rejected)
	assert.Equal(t, "4090_48G", resolved.Arguments["GpuType"])
	require.NotNil(t, resolved.Confirmation)
	assert.Equal(t, "4090_48G", resolved.Confirmation.Arguments["GpuType"],
		"the confirm card must show the exact string that will execute")
	assert.Equal(t, resolved.Arguments["GpuType"], resolved.Confirmation.Arguments["GpuType"])
}

func TestResolveRejectsUnknownGpuTypeWithoutGuessing(t *testing.T) {
	resolver := createResolver(t, liveCatalog())

	resolved := resolver.Resolve(createProposal("5090"))

	assert.False(t, resolved.ReadyForConfirmation)
	assert.NotEmpty(t, resolved.Rejected)
	assert.Nil(t, resolved.Arguments["GpuType"], "an unknown card must not resolve to anything")
	assert.Empty(t, resolved.DependencyFailures, "an unknown card is the user's input, not our outage")
}

// TestResolveReportsCatalogOutageAsDependencyFailure pins the four refusal
// channels staying distinct. A failed catalog query is OUR problem: it must not
// arrive as Rejected (which reads as "you named a bad card") and must never pass
// the gate.
func TestResolveReportsCatalogOutageAsDependencyFailure(t *testing.T) {
	resolver := createResolver(t, MachineTypeCatalog{Available: false})

	resolved := resolver.Resolve(createProposal("4090"))

	assert.False(t, resolved.ReadyForConfirmation,
		"an unconfirmable machine type must never reach the confirmation gate")
	require.Len(t, resolved.DependencyFailures, 1)
	assert.Contains(t, resolved.DependencyFailures[0], "GpuType")
	assert.Empty(t, resolved.Rejected, "a server-side outage must not be blamed on the user's value")
	assert.Nil(t, resolved.Arguments["GpuType"])
	assert.Nil(t, resolved.Confirmation)
}

func TestResolveReportsGpuAmbiguityAsConflict(t *testing.T) {
	resolver := createResolver(t, MachineTypeCatalog{Names: []string{"4090_48G", "4090-48G"}, Available: true})

	resolved := resolver.Resolve(createProposal("4090 48G"))

	assert.False(t, resolved.ReadyForConfirmation)
	require.Len(t, resolved.Conflicts, 1)
	assert.Equal(t, "GpuType", resolved.Conflicts[0].Slot)
	assert.ElementsMatch(t, []string{"4090_48G", "4090-48G"}, resolved.Conflicts[0].CatalogCandidates)
	assert.Nil(t, resolved.Arguments["GpuType"])
}
