package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSpotCatalogScopeSurvivesArgFiltering guards a parameter that was set and
// silently discarded.
//
// The availability catalog is per resource pool: InstanceType=spot lists what is
// offered on Spot, and omitting it lists on-demand. stepQueryInstanceTypes has
// set that argument for a Spot create since it was written — and because
// InstanceType is not a property of the model-facing schema, it was never in
// AllowedParams, so filterSafeArgs dropped it on every call. Nothing errored.
// The workflow simply asked about a pool it was not buying from, and the answer
// looked entirely normal.
//
// It stays internal-only on purpose: which pool to list follows from the charge
// type the create already carries, so it is derived server-side, never a choice
// the model gets to make.
func TestSpotCatalogScopeSurvivesArgFiltering(t *testing.T) {
	policies := DefaultToolExecutionPolicies()
	policy, ok := policies["DescribeAvailableCompShareInstanceTypes"]
	require.True(t, ok)

	args := map[string]any{"Zone": "cn-wlcb-01", "InstanceType": "spot"}

	internal := filterSafeArgs(args, allowedParamsForOrigin(policy, OriginWorkflowInternal))
	assert.Equal(t, "spot", internal["InstanceType"],
		"a server-side caller scoping the catalog to Spot must actually send it")
	assert.Equal(t, "cn-wlcb-01", internal["Zone"], "the ordinary parameters are unaffected")

	fromModel := filterSafeArgs(args, allowedParamsForOrigin(policy, OriginDirectLLM))
	assert.NotContains(t, fromModel, "InstanceType",
		"the model must not pick a resource pool; it follows from the create's charge type")
}

// TestInstanceTypeScopeIsNotGrantedToOtherActions keeps the grant narrow. An
// internal-only parameter is an exception to the allowlist, and an exception that
// spreads by accident is how allowlists stop meaning anything.
func TestInstanceTypeScopeIsNotGrantedToOtherActions(t *testing.T) {
	policies := DefaultToolExecutionPolicies()
	for action, policy := range policies {
		if action == "DescribeAvailableCompShareInstanceTypes" {
			continue
		}
		assert.NotContains(t, allowedParamsForOrigin(policy, OriginWorkflowInternal), "InstanceType",
			"%s must not silently gain the catalog-scope parameter", action)
	}
}
