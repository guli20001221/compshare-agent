package engine

import (
	"sort"
	"testing"

	openai "github.com/sashabaranov/go-openai"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/tools"
)

// TestPlannerRequiredToolsDoNotAuthorizeDispatch is the load-bearing negative
// test for PR6: it pins the contract that IntentRoute.RequiredTools is
// validation/trace-only LLM output and MUST NOT influence which tools the
// dispatch path exposes.
//
// It calls the SAME production seam the ReAct loop uses for req.Tools —
// visibleRegistryForIntentRoute (engine.go) — passing routes whose Intent is
// fixed but whose RequiredTools varies adversarially. The expected window
// (`want`) is computed INDEPENDENTLY of that seam, straight from the intent:
//
//	tools.VisibleRegistryForSubset(intent.IntentToolSubset(intent), mutating)
//
// so `want` is an oracle, not a tautology. If anyone rewired the seam to read
// route.RequiredTools (e.g. VisibleRegistryForSubset(route.RequiredTools, …)),
// `got` would diverge from the oracle and this test would fail — which is the
// regression guard the contract needs.
func TestPlannerRequiredToolsDoNotAuthorizeDispatch(t *testing.T) {
	// Each intent below has a non-empty dispatch subset. For each we hold the
	// intent fixed and vary plan.RequiredTools across values an adversarial /
	// sloppy LLM might emit — including the empty set, a valid-but-narrower set,
	// and outright garbage tool names. The authoritative window must be invariant.
	cases := []struct {
		probe         intent.Intent
		requiredTools [][]string
	}{
		{
			probe: intent.IntentMonitorQuery,
			requiredTools: [][]string{
				nil,
				// Validator-passing but narrower than the subset (omits the monitor tool).
				{"DescribeCompShareInstance"},
				// Garbage the LLM could hallucinate. Never validated at dispatch time.
				{"DeleteEverything", "SomeHallucinatedTool"},
			},
		},
		{
			probe: intent.IntentOperationLifecycle,
			requiredTools: [][]string{
				nil,
				// Exactly what the few-shots emit — yet the dispatch subset is 18 tools.
				{"DescribeCompShareInstance"},
				// A tool from a different intent's surface; still must not matter.
				{"CheckCompShareNetOptimizer"},
			},
		},
		{
			probe: intent.IntentResourceInfo,
			requiredTools: [][]string{
				nil,
				{"DescribeCompShareInstance"},
				{"GetCompShareInstancePrice", "bogus"},
			},
		},
	}

	for _, mutating := range []bool{false, true} {
		for _, tc := range cases {
			// The authoritative window: intent → subset → registry. This is the
			// single source of truth the engine actually dispatches against.
			want := toolNameSet(tools.VisibleRegistryForSubset(intent.IntentToolSubset(tc.probe), mutating))
			if len(want) == 0 {
				t.Fatalf("precondition: dispatch window for %q (mutating=%v) is empty; pick an intent with a subset", tc.probe, mutating)
			}

			for _, rt := range tc.requiredTools {
				// Drive the actual production seam with an adversarial RequiredTools.
				// The seam must ignore route.RequiredTools and return the oracle window.
				route := intent.IntentRoute{Intent: tc.probe, RequiredTools: rt}
				got := toolNameSet(visibleRegistryForIntentRoute(route, mutating))

				if !equalStringSets(want, got) {
					t.Errorf("dispatch window changed with RequiredTools=%v for intent %q (mutating=%v):\n  want %v\n  got  %v\nRequiredTools must not authorize dispatch.",
						rt, tc.probe, mutating, sortedKeys(want), sortedKeys(got))
				}
			}
		}
	}
}

// TestRequiredToolsWindowDivergesFromAuthoritativeWindow makes the negative test
// bite: it proves the two windows are genuinely different, so the invariance
// above is not vacuous. For operation_lifecycle, the only validator-passing
// RequiredTools the planner can emit is {DescribeCompShareInstance} (its
// requiredToolsForIntent allowlist), yet the authoritative dispatch subset
// includes mutating workflow tools that can never appear in RequiredTools (they
// are not even in validRequiredTool). Had dispatch keyed on RequiredTools, those
// workflow tools would silently vanish from the model's tool surface.
func TestRequiredToolsWindowDivergesFromAuthoritativeWindow(t *testing.T) {
	const probe = intent.IntentOperationLifecycle

	// What the few-shots actually emit and the validator accepts.
	route := intent.IntentRoute{Intent: probe, RequiredTools: []string{"DescribeCompShareInstance"}}

	// authoritative goes through the production seam; ifMiswired is the hypothetical
	// RequiredTools-keyed window the seam must NOT produce.
	authoritative := toolNameSet(visibleRegistryForIntentRoute(route, true))
	ifMiswired := toolNameSet(tools.VisibleRegistryForSubset(route.RequiredTools, true))

	if equalStringSets(authoritative, ifMiswired) {
		t.Fatalf("expected the authoritative (intent-derived) window to differ from the RequiredTools-derived window; both = %v", sortedKeys(authoritative))
	}

	// Concretely: a mutating workflow tool is in the authoritative window but can
	// never be in any plan.RequiredTools, proving authorization is intent-derived.
	const workflowTool = "StopInstanceWorkflow"
	if _, ok := authoritative[workflowTool]; !ok {
		t.Fatalf("precondition: %q expected in mutating operation_lifecycle window; got %v", workflowTool, sortedKeys(authoritative))
	}
	if _, ok := ifMiswired[workflowTool]; ok {
		t.Errorf("%q leaked into the RequiredTools-derived window; RequiredTools cannot contain workflow tools", workflowTool)
	}
}

func toolNameSet(ts []openai.Tool) map[string]struct{} {
	out := make(map[string]struct{}, len(ts))
	for _, tool := range ts {
		if tool.Function == nil {
			continue
		}
		out[tool.Function.Name] = struct{}{}
	}
	return out
}

func equalStringSets(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
