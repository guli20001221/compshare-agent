package intent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlannedRuntimeFormForIntent_ExhaustiveRuntimeIntents(t *testing.T) {
	seen := make(map[Intent]struct{}, len(RuntimeIntents()))
	for _, i := range RuntimeIntents() {
		form := PlannedRuntimeFormForIntent(i)
		require.True(t, IsRuntimeForm(form), "intent %q mapped to invalid runtime form %q", i, form)
		seen[i] = struct{}{}
	}

	require.Len(t, seen, len(RuntimeIntents()), "RuntimeIntents must not contain duplicates")
	for _, i := range RuntimeIntents() {
		assert.Contains(t, seen, i)
	}
}

func TestPlannedRuntimeFormForIntent_RuntimeIntentPartition(t *testing.T) {
	expected := map[RuntimeForm][]Intent{
		RuntimeFormRouting: {
			IntentMonitorQuery,
			IntentResourceInfo,
			IntentGPUSpecsQuery,
			IntentStockAvailability,
			IntentNetAcceleratorStatus,
			IntentImageTagCatalog,
			IntentModelRepositoryBrowse,
			IntentPlatformImageList,
			IntentCustomImageList,
			IntentCommunityImageList,
			IntentPricingQuery,
		},
		RuntimeFormTerminalRAG: {
			IntentKnowledgeQA,
		},
		RuntimeFormAgent: {
			IntentMonitorHistory,
			IntentBillingInstance,
			IntentBillingAccountUnsupported,
			IntentExpiryRenewal,
			IntentDiagnosis,
			IntentVagueFailure,
			IntentOperationLifecycle,
			IntentRecommendation,
			IntentDiskInfo,
			IntentDeployModel,
			IntentUnknown,
		},
	}

	actual := map[RuntimeForm][]Intent{}
	for _, i := range RuntimeIntents() {
		actual[PlannedRuntimeFormForIntent(i)] = append(actual[PlannedRuntimeFormForIntent(i)], i)
	}
	for form, expectedIntents := range expected {
		assert.ElementsMatch(t, expectedIntents, actual[form], form)
	}
	assert.Len(t, actual, len(expected))
}

func TestPlannedRuntimeFormForIntent_RoutingWorkflowIntents(t *testing.T) {
	for _, i := range []Intent{
		IntentMonitorQuery,
		IntentResourceInfo,
		IntentGPUSpecsQuery,
		IntentStockAvailability,
		IntentNetAcceleratorStatus,
		IntentImageTagCatalog,
		IntentModelRepositoryBrowse,
		IntentPlatformImageList,
		IntentCustomImageList,
		IntentCommunityImageList,
		IntentPricingQuery,
	} {
		assert.Equal(t, RuntimeFormRouting, PlannedRuntimeFormForIntent(i), i)
	}
}

func TestPlannedRuntimeFormForIntent_TerminalRAGIntent(t *testing.T) {
	assert.Equal(t, RuntimeFormTerminalRAG, PlannedRuntimeFormForIntent(IntentKnowledgeQA))
}

func TestPlannedRuntimeFormForIntent_AgentDefault(t *testing.T) {
	for _, i := range []Intent{
		IntentMonitorHistory,
		IntentBillingInstance,
		IntentBillingAccountUnsupported,
		IntentExpiryRenewal,
		IntentDiagnosis,
		IntentVagueFailure,
		IntentOperationLifecycle,
		IntentRecommendation,
		IntentDiskInfo,
		IntentDeployModel,
		IntentUnknown,
		Intent("made_up_intent"),
	} {
		assert.Equal(t, RuntimeFormAgent, PlannedRuntimeFormForIntent(i), i)
	}
}
