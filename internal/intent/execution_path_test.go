package intent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlannedExecutionPathForIntent_ExhaustiveRuntimeIntents(t *testing.T) {
	seen := make(map[Intent]struct{}, len(RuntimeIntents()))
	for _, i := range RuntimeIntents() {
		form := PlannedExecutionPathForIntent(i)
		require.True(t, IsExecutionPath(form), "intent %q mapped to invalid runtime form %q", i, form)
		seen[i] = struct{}{}
	}

	require.Len(t, seen, len(RuntimeIntents()), "RuntimeIntents must not contain duplicates")
	for _, i := range RuntimeIntents() {
		assert.Contains(t, seen, i)
	}
}

func TestPlannedExecutionPathForIntent_RuntimeIntentPartition(t *testing.T) {
	expected := map[ExecutionPath][]Intent{
		ExecutionPathRouting: {
			IntentMonitorQuery,
			IntentBillingAccountUnsupported,
			IntentResourceInfo,
			IntentGPUSpecsQuery,
			IntentStockAvailability,
			IntentNetAcceleratorStatus,
			IntentRefundEstimate,
			IntentCFSInfo,
			IntentImageTagCatalog,
			IntentModelRepositoryBrowse,
			IntentImageList,
			IntentPricingQuery,
		},
		ExecutionPathAgent: {
			IntentMonitorHistory,
			IntentBillingInstance,
			IntentDiagnosis,
			IntentVagueFailure,
			IntentOperationLifecycle,
			IntentKnowledgeQA,
			IntentDiskInfo,
			IntentDeployModel,
			IntentCreateInstance,
			IntentUnknown,
		},
	}

	actual := map[ExecutionPath][]Intent{}
	for _, i := range RuntimeIntents() {
		actual[PlannedExecutionPathForIntent(i)] = append(actual[PlannedExecutionPathForIntent(i)], i)
	}
	for form, expectedIntents := range expected {
		assert.ElementsMatch(t, expectedIntents, actual[form], form)
	}
	assert.Len(t, actual, len(expected))
}

func TestPlannedExecutionPathForIntent_RoutingWorkflowIntents(t *testing.T) {
	for _, i := range []Intent{
		IntentMonitorQuery,
		IntentBillingAccountUnsupported,
		IntentResourceInfo,
		IntentGPUSpecsQuery,
		IntentStockAvailability,
		IntentNetAcceleratorStatus,
		IntentRefundEstimate,
		IntentCFSInfo,
		IntentImageTagCatalog,
		IntentModelRepositoryBrowse,
		IntentImageList,
		IntentPricingQuery,
	} {
		assert.Equal(t, ExecutionPathRouting, PlannedExecutionPathForIntent(i), i)
	}
}

func TestPlannedExecutionPathForIntent_AgentDefault(t *testing.T) {
	for _, i := range []Intent{
		IntentMonitorHistory,
		IntentBillingInstance,
		IntentDiagnosis,
		IntentVagueFailure,
		IntentOperationLifecycle,
		IntentKnowledgeQA,
		IntentDiskInfo,
		IntentDeployModel,
		IntentCreateInstance,
		IntentUnknown,
		Intent("made_up_intent"),
	} {
		assert.Equal(t, ExecutionPathAgent, PlannedExecutionPathForIntent(i), i)
	}
}
