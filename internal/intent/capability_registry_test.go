package intent

import (
	"context"
	"strings"
	"testing"
)

// TestIsCapabilityIntent_KnownLabels verifies all 5 registered capability intents
// return true. New capabilities must be picked up by IsCapabilityIntent without
// any code change in callers (engine.go etc.) — this is the v1 contract.
func TestIsCapabilityIntent_KnownLabels(t *testing.T) {
	wanted := []Intent{
		IntentGPUSpecsQuery,
		IntentStockAvailability,
		IntentPlatformImageList,
		IntentCustomImageList,
		IntentCommunityImageList,
	}
	for _, intent := range wanted {
		if !IsCapabilityIntent(intent) {
			t.Errorf("IsCapabilityIntent(%q) = false, want true", intent)
		}
	}
}

// TestIsCapabilityIntent_UnknownReturnsFalse guards against accidental "capture
// everything" predicates that would break the routing OR-list in engine.go.
func TestIsCapabilityIntent_UnknownReturnsFalse(t *testing.T) {
	notCapability := []Intent{
		IntentResourceInfo,
		IntentMonitorQuery,
		IntentKnowledgeQA,
		IntentBillingInstance,
		IntentBillingAccountUnsupported,
		IntentDiagnosis,
		IntentUnknown,
		Intent("not_a_real_intent"),
	}
	for _, intent := range notCapability {
		if IsCapabilityIntent(intent) {
			t.Errorf("IsCapabilityIntent(%q) = true, want false", intent)
		}
	}
}

// TestCapabilityRegistry_NoDuplicateIntents ensures the registry table has no
// shadowed entries (a duplicate would silently mask the second handler).
func TestCapabilityRegistry_NoDuplicateIntents(t *testing.T) {
	seen := map[Intent]struct{}{}
	for _, e := range capabilityRegistry {
		if _, ok := seen[e.intent]; ok {
			t.Errorf("duplicate intent %q in capabilityRegistry", e.intent)
		}
		seen[e.intent] = struct{}{}
	}
}

// TestCapabilityRegistry_BindsToRealTool guards against typo'd tool names that
// would lookup-miss in handlerActionWhitelist or fail at SafeToolExecutor.
func TestCapabilityRegistry_BindsToRealTool(t *testing.T) {
	expected := map[Intent]string{
		IntentGPUSpecsQuery:      "DescribeAvailableCompShareInstanceTypes",
		IntentStockAvailability:  "DescribeAvailableCompShareInstanceTypes",
		IntentPlatformImageList:  "DescribeCompShareImages",
		IntentCustomImageList:    "DescribeCompShareCustomImages",
		IntentCommunityImageList: "DescribeCommunityImages",
	}
	for _, e := range capabilityRegistry {
		want, ok := expected[e.intent]
		if !ok {
			t.Errorf("unexpected intent %q in registry", e.intent)
			continue
		}
		if e.requiredTool != want {
			t.Errorf("registry[%q].requiredTool = %q, want %q", e.intent, e.requiredTool, want)
		}
	}
}

// TestHandlerActionWhitelist_DerivesFromRegistry enforces single-source-of-truth
// (memory: feedback_cross_pr_contract_drift_check). If a new capability is
// added to the registry, the whitelist must auto-include it; nothing should be
// hardcoded twice.
func TestHandlerActionWhitelist_DerivesFromRegistry(t *testing.T) {
	wl := handlerActionWhitelist()
	for _, e := range capabilityRegistry {
		actions, ok := wl[e.intent]
		if !ok {
			t.Errorf("registry intent %q missing from handlerActionWhitelist (derivation bug)", e.intent)
			continue
		}
		if _, ok := actions[e.requiredTool]; !ok {
			t.Errorf("registry[%q].requiredTool=%q not in whitelist[%q]", e.intent, e.requiredTool, e.intent)
		}
	}
}

// TestCapabilityPromptFragments_ContainsAllIntents ensures every registered
// intent has both a directive AND a planner one-shot example. Missing either
// = planner LLM unaware of the intent enum → routing degrades silently.
func TestCapabilityPromptFragments_ContainsAllIntents(t *testing.T) {
	directives, examples := CapabilityPromptFragments()
	combined := strings.Join(append(append([]string{}, directives...), examples...), "\n")
	for _, e := range capabilityRegistry {
		if !strings.Contains(combined, string(e.intent)) {
			t.Errorf("capability fragments missing intent label %q (planner won't know to emit it)", e.intent)
		}
	}
}

// TestDispatchCapability_RoutesToHandler verifies each handler is reachable via
// DispatchCapability. Uses a stub executor that fails fast so we only check
// handler routing, not full tool semantics.
type stubFailingExecutor struct{}

func (stubFailingExecutor) Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	return map[string]any{}, nil
}

func TestDispatchCapability_RoutesToHandler(t *testing.T) {
	h := NewDemoHandler(stubFailingExecutor{})
	for _, e := range capabilityRegistry {
		req := HandlerRequest{Plan: Plan{Intent: e.intent}}
		result := h.DispatchCapability(context.Background(), req)
		// With empty mock response, handlers should return a HandledResult
		// (their renderers produce "未获取到..." replies on empty data).
		if result.Status != HandlerStatusHandled {
			t.Errorf("DispatchCapability(%q) status = %q, want %q", e.intent, result.Status, HandlerStatusHandled)
		}
		if result.ToolAction != e.requiredTool {
			t.Errorf("DispatchCapability(%q) ToolAction = %q, want %q", e.intent, result.ToolAction, e.requiredTool)
		}
	}
}

// TestDispatchCapability_UnknownIntentFalls verifies that calling
// DispatchCapability with a non-registered intent returns a FallbackBeforeTool
// (defensive layer; engine.go gates on IsCapabilityIntent before invoking).
func TestDispatchCapability_UnknownIntentFalls(t *testing.T) {
	h := NewDemoHandler(stubFailingExecutor{})
	req := HandlerRequest{Plan: Plan{Intent: Intent("not_a_capability")}}
	result := h.DispatchCapability(context.Background(), req)
	if result.Status != HandlerStatusFallbackBeforeTool {
		t.Errorf("unknown-intent dispatch status = %q, want %q", result.Status, HandlerStatusFallbackBeforeTool)
	}
}

// TestCapabilityMetadata_LoadedAtBuild verifies the embed.FS frontmatter parser
// produced one entry per registry intent. Fail-fast at init() would have already
// panicked, but this test makes the requirement visible in the test report.
func TestCapabilityMetadata_LoadedAtBuild(t *testing.T) {
	if got, want := len(capabilityMetadata), len(capabilityRegistry); got != want {
		t.Fatalf("capabilityMetadata count = %d, want %d (registry size)", got, want)
	}
	regSet := map[Intent]struct{}{}
	for _, e := range capabilityRegistry {
		regSet[e.intent] = struct{}{}
	}
	for _, m := range capabilityMetadata {
		if _, ok := regSet[Intent(m.IntentLabel)]; !ok {
			t.Errorf("capabilityMetadata has intent_label %q with no matching registry entry", m.IntentLabel)
		}
		if m.RequiredCitation {
			t.Errorf("capability %q has required_citation=true; capabilities are NOT cited per PR A spec", m.Name)
		}
	}
}

// TestRegistry_FutureProof_AcceptanceNumberEight is the §5 #8 acceptance test:
// adding a capability must NOT require any change to engine.go. We simulate
// this by exercising the registry surface that engine.go depends on
// (IsCapabilityIntent + DispatchCapability), with a temporary entry, and verify
// the surface picks it up without engine.go knowing the intent's name.
//
// This is a function-scope insertion (not a permanent registry mutation): if
// any test runs concurrently with a real production engine, isolation is
// preserved because the registry is a package-level slice and Go test execution
// within one package is single-threaded by default.
func TestRegistry_FutureProof_AcceptanceNumberEight(t *testing.T) {
	const mockIntent = Intent("__test_future_proof_mock__")
	original := capabilityRegistry
	t.Cleanup(func() { capabilityRegistry = original })
	called := false
	mockHandler := func(ctx context.Context, h *DemoHandler, req HandlerRequest) HandlerResult {
		called = true
		return HandlerResult{
			Status:        HandlerStatusHandled,
			Reply:         "mock OK",
			CutoverStatus: CutoverStatusDispatched,
			ToolAction:    "MockTool",
		}
	}
	capabilityRegistry = append(append([]capabilityEntry{}, original...), capabilityEntry{
		intent:       mockIntent,
		requiredTool: "MockTool",
		handler:      mockHandler,
	})

	// The engine.go dispatch surface uses ONLY these two functions to decide
	// "is this a capability intent? if so, hand it to the registry". Both must
	// pick up the new entry without engine.go changing.
	if !IsCapabilityIntent(mockIntent) {
		t.Fatal("future-proof: IsCapabilityIntent did not pick up new registry entry")
	}
	h := NewDemoHandler(stubFailingExecutor{})
	result := h.DispatchCapability(context.Background(), HandlerRequest{Plan: Plan{Intent: mockIntent}})
	if !called {
		t.Fatal("future-proof: DispatchCapability did not invoke mock handler")
	}
	if result.Status != HandlerStatusHandled || result.Reply != "mock OK" {
		t.Fatalf("future-proof: handler result = %+v, want handled with mock reply", result)
	}
}
