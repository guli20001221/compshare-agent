package tools

import (
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func hasTool(tools []openai.Tool, name string) bool {
	for _, t := range tools {
		if t.Function != nil && t.Function.Name == name {
			return true
		}
	}
	return false
}

// TestSearchKnowledgeGatedVisibility pins the P3 gating mechanism (design 2):
// COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE is the single tool-visibility gate. Flag off
// => SearchKnowledge invisible for EVERY surface (full read-only, mutating, and
// any subset), so the tool surface is byte-identical to before it existed. Flag
// on => present in full-registry surfaces and in subsets that list it; subset
// scoping still excludes it from subsets that do not.
func TestSearchKnowledgeGatedVisibility(t *testing.T) {
	defer SetAgenticSearchKnowledgeEnabled(false) // restore default for other tests

	SetAgenticSearchKnowledgeEnabled(false)
	assert.False(t, hasTool(VisibleRegistry(false), "SearchKnowledge"), "off: read-only surface")
	assert.False(t, hasTool(VisibleRegistry(true), "SearchKnowledge"), "off: mutating surface")
	assert.False(t, hasTool(VisibleRegistryForSubset([]string{"SearchKnowledge", "DescribeCompShareInstance"}, false), "SearchKnowledge"), "off: subset listing it")

	SetAgenticSearchKnowledgeEnabled(true)
	assert.True(t, hasTool(VisibleRegistry(false), "SearchKnowledge"), "on: read-only (read_cheap survives the filter)")
	assert.True(t, hasTool(VisibleRegistry(true), "SearchKnowledge"), "on: mutating surface")
	assert.Equal(t, len(Registry), len(VisibleRegistry(true)), "on: mutating shows the full registry incl SearchKnowledge")
	assert.True(t, hasTool(VisibleRegistryForSubset([]string{"SearchKnowledge", "DescribeCompShareInstance"}, false), "SearchKnowledge"), "on: subset listing it")
	assert.False(t, hasTool(VisibleRegistryForSubset([]string{"DescribeCompShareInstance"}, false), "SearchKnowledge"), "on: subset NOT listing it (subset scoping holds)")
}

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
