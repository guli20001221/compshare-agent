package intent

import "testing"

// TestNewPlannerDelegatesToNewIntentRouter pins the #161 contract: NewPlanner is
// now a pure deprecated shim over NewIntentRouter, so both constructors must
// produce an identically-configured router for the same options, and both must
// apply the same defaults (capability lookup + minimum one retry). If someone
// later diverges the two constructors, this fails.
func TestNewPlannerDelegatesToNewIntentRouter(t *testing.T) {
	// A nil client is fine: constructors never call the LLM, they only wire fields.
	var client PlannerLLM
	opts := PlannerOptions{BaseURL: "https://example.invalid", Model: "ds-v4-flash", MaxRetries: 3}

	router := NewIntentRouter(client, opts)
	shim := NewPlanner(client, opts)

	// *Planner is an alias of *IntentRouter, so the shim must return the same
	// shape with identical explicit-option wiring.
	if router.baseURL != shim.baseURL || router.model != shim.model || router.maxRetries != shim.maxRetries {
		t.Fatalf("NewPlanner diverged from NewIntentRouter:\n  router=%+v\n  shim=  %+v", router, shim)
	}
	if router.baseURL != opts.BaseURL || router.model != opts.Model {
		t.Errorf("explicit BaseURL/Model not wired: got baseURL=%q model=%q", router.baseURL, router.model)
	}
	if router.maxRetries != 3 {
		t.Errorf("explicit MaxRetries not honored: got %d, want 3", router.maxRetries)
	}

	// Defaults: zero MaxRetries → 1, and a nil LookupCapability → a non-nil
	// default. Assert through both constructors so the shim can't skip them.
	for name, r := range map[string]*IntentRouter{
		"NewIntentRouter": NewIntentRouter(client, PlannerOptions{}),
		"NewPlanner":      NewPlanner(client, PlannerOptions{}),
	} {
		if r.maxRetries != 1 {
			t.Errorf("%s: default maxRetries = %d, want 1", name, r.maxRetries)
		}
		if r.lookupCapability == nil {
			t.Errorf("%s: default lookupCapability not applied (nil)", name)
		}
	}
}
