package intent

import "testing"

// TestNewIntentRouterWiresOptionsAndDefaults pins NewIntentRouter's wiring
// contract: explicit options (BaseURL/Model/MaxRetries) are honored verbatim,
// and the two defaults (zero MaxRetries → 1, nil LookupCapability → a non-nil
// default) are applied. This replaces the former NewPlanner→NewIntentRouter
// delegation test: the deprecated NewPlanner shim was removed in the
// planner→intent-router rename (#130 Stage 1), so the only constructor left to
// pin is NewIntentRouter itself.
func TestNewIntentRouterWiresOptionsAndDefaults(t *testing.T) {
	// A nil client is fine: the constructor never calls the LLM, it only wires fields.
	var client IntentRouterLLM
	opts := IntentRouterOptions{BaseURL: "https://example.invalid", Model: "ds-v4-flash", MaxRetries: 3}

	router := NewIntentRouter(client, opts)

	if router.baseURL != opts.BaseURL || router.model != opts.Model {
		t.Errorf("explicit BaseURL/Model not wired: got baseURL=%q model=%q", router.baseURL, router.model)
	}
	if router.maxRetries != 3 {
		t.Errorf("explicit MaxRetries not honored: got %d, want 3", router.maxRetries)
	}

	// Defaults: zero MaxRetries → 1, and a nil LookupCapability → a non-nil default.
	defaulted := NewIntentRouter(client, IntentRouterOptions{})
	if defaulted.maxRetries != 1 {
		t.Errorf("default maxRetries = %d, want 1", defaulted.maxRetries)
	}
	if defaulted.lookupCapability == nil {
		t.Errorf("default lookupCapability not applied (nil)")
	}
}
