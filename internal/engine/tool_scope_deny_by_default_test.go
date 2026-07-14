package engine

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/tools"
)

// The router's job is to decide what the user wants. When it CANNOT decide, the one
// thing the agent must not gain is the power to change the user's infrastructure.
//
// Before this change it gained exactly that. intent.IntentToolSubset returns nil for any
// intent without an explicit allowlist, and tools.VisibleRegistryForSubset read a nil
// subset as "no filter — show everything", including every mutating workflow tool, because
// production runs with mutating enabled. The safety property was inverted: maximum
// uncertainty bought maximum privilege.
//
// These tests are written against the REAL production seam (visibleRegistryForIntentRoute,
// the function engine.go passes to req.Tools), not a reconstruction of it.

// mutatingToolNames returns the workflow tools that actually change a user's resources.
// Derived from the live registry rather than hardcoded, so a newly added mutating tool is
// covered by these gates the day it lands instead of the day someone remembers to update a
// list here.
func mutatingToolNames(t *testing.T) map[string]struct{} {
	t.Helper()
	readOnly := toolNameSet(tools.VisibleRegistry(false))
	full := toolNameSet(tools.VisibleRegistry(true))

	mutating := make(map[string]struct{})
	for name := range full {
		if _, isRead := readOnly[name]; !isRead {
			mutating[name] = struct{}{}
		}
	}
	require.NotEmpty(t, mutating,
		"precondition: enabling mutating tools must expose tools the read-only registry hides; "+
			"if this is empty the test below can pass vacuously")
	return mutating
}

// THE GATE. An intent the router could not resolve must never see a tool that writes.
//
// intent.Intent is a string type, so an unrouted turn can carry the empty intent, a
// well-known "unknown", or — when a future route is added without an allowlist — a label
// this code has never heard of. All three must fail closed.
func TestAnUnroutedTurnIsNeverAuthorizedToWrite(t *testing.T) {
	mutating := mutatingToolNames(t)

	unrouted := []intent.Intent{
		intent.Intent(""),        // the router returned nothing at all
		intent.Intent("unknown"), // the router explicitly gave up
		intent.Intent("some_route_someone_adds_in_2027_and_forgets_to_scope"),
	}

	for _, i := range unrouted {
		// mutatingEnabled=true is PRODUCTION. Passing false here would make this test
		// pass for the wrong reason — the registry would hide the write tools anyway.
		window := toolNameSet(visibleRegistryForIntentRoute(intent.IntentRoute{Intent: i}, true))

		require.NotEmpty(t, window,
			"intent %q: an unrouted turn still answers follow-ups, so it must keep its READ tools", i)

		for name := range window {
			require.NotContains(t, mutating, name,
				"intent %q was handed the mutating tool %q. The router did not know what the user "+
					"wanted, and we gave the model the power to stop their instance.", i, name)
		}
	}
}

func TestSearchKnowledgeIsVisibleOnlyToKnowledgeQA(t *testing.T) {
	previous := tools.AgenticSearchKnowledgeEnabled()
	tools.SetAgenticSearchKnowledgeEnabled(true)
	t.Cleanup(func() { tools.SetAgenticSearchKnowledgeEnabled(previous) })

	require.Contains(t, toolNameSet(visibleRegistryForIntentRoute(
		intent.IntentRoute{Intent: intent.IntentKnowledgeQA}, true)), "SearchKnowledge")

	for _, i := range []intent.Intent{
		intent.IntentDiagnosis,
		intent.IntentVagueFailure,
		intent.IntentUnknown,
		intent.Intent(""),
		intent.Intent("some_route_someone_adds_in_2027_and_forgets_to_scope"),
	} {
		require.NotContains(t, toolNameSet(visibleRegistryForIntentRoute(
			intent.IntentRoute{Intent: i}, true)), "SearchKnowledge",
			"intent %q must not bypass the knowledge_qa evidence-verification exit", i)
	}
}

// The negative control. If the fix were "hide mutating tools from everyone", the gate above
// would pass and the product would be broken — a lifecycle turn genuinely needs to stop an
// instance. operation_lifecycle has an explicit allowlist that names workflow tools, and it
// must still receive them.
//
// Without this, "deny by default" could be satisfied by denying everything.
func TestAnIntentThatIsAuthorizedToWriteStillCan(t *testing.T) {
	window := toolNameSet(visibleRegistryForIntentRoute(
		intent.IntentRoute{Intent: intent.IntentOperationLifecycle}, true))

	require.Contains(t, window, "StopInstanceWorkflow",
		"operation_lifecycle names StopInstanceWorkflow in its allowlist; deny-by-default must not "+
			"strip an authorization the intent explicitly carries")
}

// Deny-by-default must not silently RESHAPE the windows that were already explicit. Every
// intent with an allowlist must come back byte-identical to the allowlist-driven window,
// otherwise this "security fix" is quietly a behavior change on routed traffic too.
//
// `want` is computed independently of the seam, straight from the intent's own subset, so it
// is an oracle rather than a restatement of the implementation.
func TestExplicitAllowlistsDoNotDrift(t *testing.T) {
	for _, i := range intent.AllIntents() {
		subset := intent.IntentToolSubset(i)
		if len(subset) == 0 {
			continue // the unrouted case; covered by the gate above
		}
		for _, mutating := range []bool{false, true} {
			want := toolNameSet(tools.VisibleRegistryForSubset(subset, mutating))
			got := toolNameSet(visibleRegistryForIntentRoute(intent.IntentRoute{Intent: i}, mutating))
			require.Equal(t, want, got,
				"intent %q (mutating=%v): the tool window changed for an intent that already had an "+
					"explicit allowlist", i, mutating)
		}
	}
}

// The sentinel itself. ToolScopeNamed with an empty Names list authorizes NOTHING — it is
// not a synonym for "everything". This is the precise inversion the old nil subset encoded,
// so it gets its own gate at the tools layer.
func TestAnEmptyAllowlistAuthorizesNothing(t *testing.T) {
	got := tools.VisibleRegistryForScope(
		tools.ToolScope{Mode: tools.ToolScopeNamed, Names: nil}, true)
	require.Empty(t, got,
		"an empty named allowlist must authorize no tools; returning the full registry here is the "+
			"original defect")
}

// A zero-valued ToolScope — the thing a caller gets by forgetting to set Mode — must fail
// closed. If the zero value handed out write access, every future struct literal that omits
// Mode would reintroduce the bug.
func TestAZeroValuedScopeCannotWrite(t *testing.T) {
	mutating := mutatingToolNames(t)
	for name := range toolNameSet(tools.VisibleRegistryForScope(tools.ToolScope{}, true)) {
		require.NotContains(t, mutating, name,
			"the zero-valued ToolScope authorized the mutating tool %q", name)
	}
}
