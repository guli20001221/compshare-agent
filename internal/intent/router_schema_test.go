package intent

import (
	"encoding/json"
	"testing"
)

// TestIntentRouteResponseSchema_IntentEnumMatchesRuntime is the default-off drift
// guard: the public default schema mirrors the default router prompt. Gated
// create_instance stays out until the caller asks for the unified-create schema.
func TestIntentRouteResponseSchema_IntentEnumMatchesRuntime(t *testing.T) {
	var schema struct {
		Type       string `json:"type"`
		Properties struct {
			SchemaVersion struct {
				Const string `json:"const"`
			} `json:"schema_version"`
			Intent struct {
				Enum []string `json:"enum"`
			} `json:"intent"`
			Confidence struct {
				Minimum *float64 `json:"minimum"`
				Maximum *float64 `json:"maximum"`
			} `json:"confidence"`
			Retrieval struct {
				Properties struct {
					Enabled struct {
						Const *bool `json:"const"`
					} `json:"enabled"`
				} `json:"properties"`
			} `json:"retrieval"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(IntentRouteResponseSchema(), &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}

	if schema.Type != "object" {
		t.Errorf("schema type = %q, want object", schema.Type)
	}
	if schema.Properties.SchemaVersion.Const != SchemaVersion {
		t.Errorf("schema_version const = %q, want %q", schema.Properties.SchemaVersion.Const, SchemaVersion)
	}

	want := make(map[string]bool, len(routerRuntimeIntents(false)))
	for _, i := range routerRuntimeIntents(false) {
		want[string(i)] = true
	}
	got := make(map[string]bool, len(schema.Properties.Intent.Enum))
	for _, e := range schema.Properties.Intent.Enum {
		got[e] = true
	}
	if len(got) != len(want) {
		t.Fatalf("intent enum size = %d, want %d (default router intents)", len(got), len(want))
	}
	for i := range want {
		if !got[i] {
			t.Errorf("intent enum is missing runtime intent %q", i)
		}
	}
	// Every enum value must be a real runtime intent (no stale extras).
	for e := range got {
		if !want[e] {
			t.Errorf("intent enum carries non-runtime intent %q", e)
		}
	}

	// Confidence bounds must match ValidateRoute's [0,1] contract.
	if schema.Properties.Confidence.Minimum == nil || *schema.Properties.Confidence.Minimum != 0 {
		t.Error("confidence minimum must be 0")
	}
	if schema.Properties.Confidence.Maximum == nil || *schema.Properties.Confidence.Maximum != 1 {
		t.Error("confidence maximum must be 1")
	}

	// retrieval.enabled must be pinned false (ValidateRoute rejects Enabled==true).
	if c := schema.Properties.Retrieval.Properties.Enabled.Const; c == nil || *c != false {
		t.Error("retrieval.enabled must be const false")
	}

	// The seven IntentRoute top-level fields ValidateRoute reads must be required.
	requiredSet := make(map[string]bool, len(schema.Required))
	for _, r := range schema.Required {
		requiredSet[r] = true
	}
	for _, field := range []string{"schema_version", "intent", "slots", "required_tools", "retrieval", "hard_block_hint", "confidence"} {
		if !requiredSet[field] {
			t.Errorf("top-level required is missing %q", field)
		}
	}
}

func TestIntentRouteResponseSchemaForIntents_CanExposeUnifiedCreate(t *testing.T) {
	var schema struct {
		Properties struct {
			Intent struct {
				Enum []string `json:"enum"`
			} `json:"intent"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(IntentRouteResponseSchemaForIntents(routerRuntimeIntents(true)), &schema); err != nil {
		t.Fatalf("schema invalid: %v", err)
	}
	got := map[string]bool{}
	for _, e := range schema.Properties.Intent.Enum {
		got[e] = true
	}
	if !got[string(IntentCreateInstance)] {
		t.Fatalf("unified-create schema must include %q", IntentCreateInstance)
	}
}

func TestIntentRouteResponseSchema_ExposesImageSourceSlot(t *testing.T) {
	var schema struct {
		Properties struct {
			Slots struct {
				Properties struct {
					ImageSource struct {
						Enum []string `json:"enum"`
					} `json:"image_source"`
				} `json:"properties"`
			} `json:"slots"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(IntentRouteResponseSchema(), &schema); err != nil {
		t.Fatalf("schema invalid: %v", err)
	}
	got := map[string]bool{}
	for _, v := range schema.Properties.Slots.Properties.ImageSource.Enum {
		got[v] = true
	}
	for _, want := range []string{"", "platform", "custom", "community", "shared"} {
		if !got[want] {
			t.Fatalf("image_source enum missing %q: %#v", want, got)
		}
	}
}

// TestIntentRouteResponseSchema_AcceptsValidatorCompatibleOutput is a coupling
// check: a representative IntentRoute the validator accepts must round-trip
// through the schema's declared structure (intent in enum, schema_version const,
// the polymorphic filter target_ref with no source/source_span). This is a
// structural sanity check, not a full JSON-Schema validation (the provider does
// the latter at request time).
func TestIntentRouteResponseSchema_AcceptsValidatorCompatibleOutput(t *testing.T) {
	// A filter target_ref deliberately omits source/source_span (they are not in
	// the item's `required`), mirroring validateTargetRef's filter exemption.
	sample := IntentRoute{
		SchemaVersion: SchemaVersion,
		Intent:        IntentResourceInfo,
		Slots: Slots{
			TargetRefs: []TargetRef{{Type: TargetRefFilter, Value: "state=running"}},
		},
		RequiredTools: []string{"DescribeCompShareInstance"},
		Retrieval:     Retrieval{Enabled: false},
		Confidence:    0.85,
	}
	if err := ValidateRoute(sample, ValidationContext{UserText: "which machines are running"}); err != nil {
		t.Fatalf("sample plan should validate: %v", err)
	}
	// The schema must list the sample's intent in its enum.
	var schema struct {
		Properties struct {
			Intent struct {
				Enum []string `json:"enum"`
			} `json:"intent"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(IntentRouteResponseSchema(), &schema); err != nil {
		t.Fatalf("schema invalid: %v", err)
	}
	found := false
	for _, e := range schema.Properties.Intent.Enum {
		if e == string(sample.Intent) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("schema intent enum does not include %q", sample.Intent)
	}
}
