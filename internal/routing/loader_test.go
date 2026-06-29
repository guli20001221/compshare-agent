package routing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const seededRoot = "."

func TestNewLoader_LoadsSeededRoutes(t *testing.T) {
	loader, err := NewLoader(seededRoot)
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	want := []string{
		"cfs_info",
		"gpu_specs_query",
		"image_list",
		"image_tag_catalog",
		"model_repository_browse",
		"network_accelerator_status",
		"pricing_query",
		"refund_estimate",
		"stock_availability",
	}
	if loader.Len() != len(want) {
		t.Fatalf("loaded %d routes, want %d (%v)", loader.Len(), len(want), loader.Names())
	}
	for _, name := range want {
		route, ok := loader.Fetch(name)
		if !ok {
			t.Errorf("route %q not loaded; got %v", name, loader.Names())
			continue
		}
		if route.IntentLabel != name {
			t.Errorf("%s: intent_label = %q, want %q", name, route.IntentLabel, name)
		}
		if route.HandlerKey == "" {
			t.Errorf("%s: handler_key is empty", name)
		}
		if route.VerificationStatus != VerificationProductionValidated {
			t.Errorf("%s: verification_status = %q, want production_validated", name, route.VerificationStatus)
		}
		if route.Provenance != ProvenanceHumanAuthored {
			t.Errorf("%s: provenance = %q, want human_authored", name, route.Provenance)
		}
	}
}

func TestSeededRoutes_UseImperativePlannerDirectives(t *testing.T) {
	loader, err := NewLoader(seededRoot)
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	banned := []string{
		"should emit",
		"should use",
		"stay in",
		"应 emit",
		"也应 emit",
		"时 emit",
		"应走",
	}
	for _, name := range loader.Names() {
		route, ok := loader.Fetch(name)
		if !ok {
			t.Fatalf("route %q disappeared", name)
		}
		for _, directive := range route.PlannerDirectives {
			for _, phrase := range banned {
				if strings.Contains(directive, phrase) {
					t.Errorf("%s directive %q contains descriptive phrase %q; use imperative wording", name, directive, phrase)
				}
			}
		}
	}
}

func TestParseRouteFile_RejectsUnknownYAMLKey(t *testing.T) {
	root := t.TempDir()
	writeRoute(t, root, "bad_route", ""+
		"name: bad_route\n"+
		"description: bad route\n"+
		"intent_label: bad_route\n"+
		"route_group: catalog\n"+
		"required_tools: [DescribeCompShareInstance]\n"+
		"tool_subset: [DescribeCompShareInstance]\n"+
		"handler_key: handleGPUSpecsQuery\n"+
		"verification_status: production_validated\n"+
		"field_refs_verified: true\n"+
		"provenance: human_authored\n"+
		"not_a_real_field: true\n")

	_, err := ParseRouteFile(filepath.Join(root, "bad_route", RouteFileName))
	if err == nil || !strings.Contains(err.Error(), "not_a_real_field") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

func TestParseRouteFile_RequiresHandlerKey(t *testing.T) {
	root := t.TempDir()
	writeRoute(t, root, "missing_handler", ""+
		"name: missing_handler\n"+
		"description: missing handler\n"+
		"intent_label: missing_handler\n"+
		"route_group: catalog\n"+
		"required_tools: [DescribeCompShareInstance]\n"+
		"tool_subset: [DescribeCompShareInstance]\n"+
		"verification_status: production_validated\n"+
		"field_refs_verified: true\n"+
		"provenance: human_authored\n")

	_, err := ParseRouteFile(filepath.Join(root, "missing_handler", RouteFileName))
	if err == nil || !strings.Contains(err.Error(), "handler_key") {
		t.Fatalf("expected handler_key error, got %v", err)
	}
}

func writeRoute(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir route dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, RouteFileName), []byte(body), 0o644); err != nil {
		t.Fatalf("write route.yaml: %v", err)
	}
}
