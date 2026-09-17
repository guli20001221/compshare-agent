package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSearchKnowledgePolicyIsLocalReadOnly proves SearchKnowledge is a read-only
// knowledge-routed tool that MUST be dispatched locally: its Route is knowledge,
// NOT external_api, so SafeToolExecutor (which only runs external_api actions)
// hard-blocks it — confirming the engine's dedicated local branch is the path.
// It must also carry a policy with AllowedParams so FilterArgs keeps query/hint.
func TestSearchKnowledgePolicyIsLocalReadOnly(t *testing.T) {
	policies := DefaultToolExecutionPolicies()
	policy, ok := policies["SearchKnowledge"]
	require.True(t, ok, "SearchKnowledge must have a policy (FilterArgs needs AllowedParams)")
	assert.Equal(t, ActionRouteKnowledge, policy.Route)
	assert.NotEqual(t, ActionRouteExternalAPI, policy.Route, "must NOT be external_api or SafeExecutor would run it")
	assert.Equal(t, ActionClassReadCheap, policy.Class)
	assert.False(t, policy.NeedsConfirm, "read-only knowledge tool needs no confirmation")
	assert.ElementsMatch(t, []string{"query", "context_hint"}, policy.AllowedParams)
}
