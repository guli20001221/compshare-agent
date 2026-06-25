package intent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntentToolSubset_DiagnosisReturnsEntryTools(t *testing.T) {
	subset := IntentToolSubset(IntentDiagnosis)
	require.Len(t, subset, 11)
	// SearchKnowledge (P4a) is a candidate diagnosis tool, emit-gated at the
	// visibility layer (tools.VisibleRegistry) behind COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE:
	// listing it here is inert until the flag is on, so the EMITTED diagnosis
	// subset is byte-identical to before when off (see internal/tools
	// TestSearchKnowledgeGatedVisibility).
	assert.Contains(t, subset, "SearchKnowledge")
	assert.Contains(t, subset, "DiagnoseSSH")
	assert.Contains(t, subset, "DiagnosePortOrFirewall")
	assert.Contains(t, subset, "DescribeCompShareInstance")
	assert.Contains(t, subset, "DescribeCompShareSoftwarePort")
	assert.Contains(t, subset, "DescribeCompShareJupyterToken")
	assert.NotContains(t, subset, "GetCompShareInstancePrice")
	assert.NotContains(t, subset, "DescribeCompShareImages")
	assert.NotContains(t, subset, "CreateInstanceWorkflow")
}

func TestIntentToolSubset_VagueFailureSameAsDiagnosis(t *testing.T) {
	assert.Equal(t, IntentToolSubset(IntentDiagnosis), IntentToolSubset(IntentVagueFailure))
}

func TestIntentToolSubset_ResourceInfo(t *testing.T) {
	subset := IntentToolSubset(IntentResourceInfo)
	require.Len(t, subset, 6)
	assert.Contains(t, subset, "DescribeCompShareInstance")
	assert.Contains(t, subset, "GetCompShareInstanceMonitor")
	assert.Contains(t, subset, "DescribeCompShareSoftwarePort")
	assert.Contains(t, subset, "DescribeCompShareJupyterToken")
	assert.Contains(t, subset, "DescribeCFS")
	assert.Contains(t, subset, "GetCompShareInstanceUserPrice")
	assert.NotContains(t, subset, "DiagnoseSSH")
}

func TestIntentToolSubset_MonitorQuery(t *testing.T) {
	subset := IntentToolSubset(IntentMonitorQuery)
	require.Len(t, subset, 2)
	assert.Contains(t, subset, "DescribeCompShareInstance")
	assert.Contains(t, subset, "GetCompShareInstanceMonitor")
}

func TestIntentToolSubset_BillingSameAsExpiryRenewal(t *testing.T) {
	assert.Equal(t, IntentToolSubset(IntentBillingInstance), IntentToolSubset(IntentExpiryRenewal))
	subset := IntentToolSubset(IntentBillingInstance)
	require.Len(t, subset, 6)
	assert.Contains(t, subset, "DescribeCompShareInstance")
	assert.Contains(t, subset, "GetCompShareInstanceUserPrice")
	assert.Contains(t, subset, "GetCompShareInstancePrice")
	assert.Contains(t, subset, "GetCompShareRefundPrice")
	assert.Contains(t, subset, "GetCompShareCFSRefundPrice")
	assert.Contains(t, subset, "DiagnoseBilling")
}

func TestIntentToolSubset_RoutingIntents(t *testing.T) {
	cases := []struct {
		intent Intent
		tool   string
		count  int
	}{
		{IntentGPUSpecsQuery, "DescribeAvailableCompShareInstanceTypes", 2},
		{IntentStockAvailability, "DescribeAvailableCompShareInstanceTypes", 5},
		{IntentPricingQuery, "GetCompShareInstancePrice", 2},
		{IntentNetAcceleratorStatus, "CheckCompShareNetOptimizer", 1},
		{IntentRefundEstimate, "GetCompShareRefundPrice", 2},
		{IntentCFSInfo, "DescribeCFS", 4},
		{IntentImageTagCatalog, "DescribeCompShareImageTags", 1},
		{IntentModelRepositoryBrowse, "DescribeModelRepositoryModels", 2},
		{IntentPlatformImageList, "DescribeCompShareImages", 1},
		{IntentCustomImageList, "DescribeCompShareCustomImages", 1},
		{IntentCommunityImageList, "DescribeCommunityImages", 1},
		{IntentSharedImageList, "DescribeCompShareSharingImages", 1},
	}
	for _, tc := range cases {
		t.Run(string(tc.intent), func(t *testing.T) {
			subset := IntentToolSubset(tc.intent)
			require.Len(t, subset, tc.count)
			assert.Contains(t, subset, tc.tool)
			if tc.intent == IntentModelRepositoryBrowse {
				assert.Contains(t, subset, "DescribeModelRepositoryTags")
			}
		})
	}
}

func TestIntentToolSubset_Recommendation(t *testing.T) {
	subset := IntentToolSubset(IntentRecommendation)
	require.Len(t, subset, 4)
	assert.Contains(t, subset, "GetGPURecommendation")
	assert.Contains(t, subset, "DescribeAvailableCompShareInstanceTypes")
	assert.Contains(t, subset, "GetCompShareInstancePrice")
	assert.Contains(t, subset, "DescribeCompShareImages")
}

func TestIntentToolSubset_OperationLifecycle(t *testing.T) {
	subset := IntentToolSubset(IntentOperationLifecycle)
	require.Len(t, subset, 23)
	assert.Contains(t, subset, "DescribeCompShareInstance")
	assert.Contains(t, subset, "CreateInstanceWorkflow")
	assert.Contains(t, subset, "StopInstanceWorkflow")
	assert.Contains(t, subset, "ResetPasswordWorkflow")
	assert.Contains(t, subset, "ResizeInstanceWorkflow")
	assert.Contains(t, subset, "ReinstallInstanceWorkflow")
	assert.Contains(t, subset, "CreateDiskWorkflow")
	assert.Contains(t, subset, "ResizeDiskWorkflow")
	assert.Contains(t, subset, "CreateCustomImageWorkflow")
	assert.Contains(t, subset, "EnableNetOptimizerWorkflow")
	assert.Contains(t, subset, "CreateCFSWorkflow")
	assert.Contains(t, subset, "ResizeCFSWorkflow")
	assert.Contains(t, subset, "GetCompShareInstanceUpgradePrice")
	assert.NotContains(t, subset, "DiagnoseSSH")
	assert.NotContains(t, subset, "GetGPURecommendation")
}

func TestIntentToolSubset_NilForAmbiguousIntents(t *testing.T) {
	for _, i := range []Intent{
		IntentUnknown,
		IntentKnowledgeQA,
		IntentMixedDiagnosisKB,
		IntentMixedBillingKB,
		IntentBillingAccountUnsupported,
		"",
	} {
		assert.Nil(t, IntentToolSubset(i), "expected nil for %q", i)
	}
}

func TestIntentToolSubset_AllRuntimeIntentsCovered(t *testing.T) {
	nilExpected := map[Intent]bool{
		IntentUnknown:                   true,
		IntentKnowledgeQA:               true,
		IntentBillingAccountUnsupported: true,
		// deploy_model has no ReAct tool subset: the engine's tryDeployModel handler
		// always handles the turn (it never falls through to the ReAct loop), so
		// there is no fallback tool list to expose (B8.3).
		IntentDeployModel: true,
	}
	for _, i := range RuntimeIntents() {
		subset := IntentToolSubset(i)
		if nilExpected[i] {
			assert.Nil(t, subset, "expected nil for %q", i)
		} else {
			assert.NotNil(t, subset, "missing tool subset for runtime intent %q", i)
		}
	}
}
