package intent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// These guard the LIVE Intent enum and RuntimeIntents(), which production still
// consumes (internal/prompt/prompt_goldens_test.go:30 reads RuntimeIntents to build
// its valid-intent set). They lived at the bottom of validator_test.go and were moved
// here when ValidateRoute and its 18 tests were deleted: the file was named after the
// validator and 18 of its 21 tests drove it, so "delete validator_test.go" looked
// obviously correct and would have silently taken three guards on live code with it.

func TestIntentEnumDeclaresAllV1Intents(t *testing.T) {
	assert.ElementsMatch(t, []Intent{
		IntentMonitorQuery,
		IntentMonitorHistory,
		IntentResourceInfo,
		IntentBillingInstance,
		IntentBillingAccountUnsupported,
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
		IntentZoneCatalog,
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
