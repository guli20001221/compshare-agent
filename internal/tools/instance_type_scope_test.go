package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTheModelCannotScopeTheAvailabilityCatalogToAPool keeps InstanceType out of
// the availability query — from every origin, including server-side callers.
//
// It reads like a parameter worth having: the create knows its charge type, so
// scoping the catalog to the Spot pool looks like the honest version of the
// query. Upstream accepts the value and answers with nothing.
// DescribeAvailableCompShareInstanceTypes appends a row only when InstanceType
// is "uhost" or "all" (uhost-compshare-api,
// ucloud/describe_available_compshare_instance_types.go formatResponse), and
// its dispatcher has no Pod branch, so "spot" is a *valid* value with an empty
// result rather than an error.
//
// Measured live against production 2026-07-22:
//
//	absent          rows=19  distinctGpu=12
//	InstanceType=uhost  rows=19  distinctGpu=12
//	InstanceType=all    rows=19  distinctGpu=12
//	InstanceType=spot   rows=0   distinctGpu=0
//
// An empty catalog is not a narrower catalog: resolveTargetSpec then fails with
// "未找到 X × N 卡的可用配比" and names no GPU types, and every guided card that
// reads 查询可用配比 renders zero options. Granting this parameter therefore breaks
// exactly the flow it was meant to make more accurate.
//
// Spot eligibility has a real source — DescribeCompShareGpuInventory returns both
// pools AND SpotUnsupportedGpuTypes, and takes no charge type to ask.
func TestTheModelCannotScopeTheAvailabilityCatalogToAPool(t *testing.T) {
	policies := DefaultToolExecutionPolicies()
	policy, ok := policies["DescribeAvailableCompShareInstanceTypes"]
	require.True(t, ok)

	args := map[string]any{"Zone": "cn-wlcb-01", "InstanceType": "spot"}

	for _, origin := range []ExecutionOrigin{OriginWorkflowInternal, OriginDirectLLM, OriginDiagnosisInternal} {
		got := filterSafeArgs(args, allowedParamsForOrigin(policy, origin))
		assert.NotContains(t, got, "InstanceType",
			"origin %v must not scope the catalog to a pool — upstream answers spot with an empty list", origin)
		assert.Equal(t, "cn-wlcb-01", got["Zone"], "the ordinary parameters are unaffected")
	}
}

// TestNoActionGrantsTheCatalogPoolParameter keeps the ban from being reopened on
// a neighbouring action. The parameter is upstream-valid everywhere it is
// accepted, so nothing errors when it is granted by mistake — the catalog just
// comes back empty and the failure surfaces several steps later as a spec that
// "does not exist".
func TestNoActionGrantsTheCatalogPoolParameter(t *testing.T) {
	for action, policy := range DefaultToolExecutionPolicies() {
		for _, origin := range []ExecutionOrigin{OriginWorkflowInternal, OriginDirectLLM, OriginDiagnosisInternal} {
			assert.NotContains(t, allowedParamsForOrigin(policy, origin), "InstanceType",
				"%s must not gain the catalog-pool parameter", action)
		}
	}
}
