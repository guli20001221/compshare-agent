package routing

import (
	"bytes"
	"os"
	"testing"
)

const generatedFile = "registry_gen.go"

func normalizeLF(b []byte) []byte {
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(b, []byte("\r"), []byte("\n"))
}

func TestGeneratedRegistry_MatchesDisk(t *testing.T) {
	want, err := GenerateRegistry(seededRoot)
	if err != nil {
		t.Fatalf("GenerateRegistry: %v", err)
	}
	got, err := os.ReadFile(generatedFile)
	if err != nil {
		t.Fatalf("read %s: %v", generatedFile, err)
	}
	if !bytes.Equal(normalizeLF(got), normalizeLF(want)) {
		t.Fatalf("%s is stale - run `go generate ./internal/routing` and commit the result", generatedFile)
	}
}

func TestGeneratedRegistry_DigestPinned(t *testing.T) {
	if generatedRegistryDigestExpected == "" {
		t.Fatal("generatedRegistryDigestExpected must be set after route registry generation")
	}
	src, err := os.ReadFile(generatedFile)
	if err != nil {
		t.Fatalf("read %s: %v", generatedFile, err)
	}
	got := computeRegistryDigest(src)
	if got != generatedRegistryDigestExpected {
		t.Fatalf("registry digest mismatch:\n  got  %s\n  want %s", got, generatedRegistryDigestExpected)
	}
}

func TestGeneratedRegistry_SemanticParityWithLoader(t *testing.T) {
	loader, err := NewLoader(seededRoot)
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	generated := GeneratedRoutes()
	if len(generated) != loader.Len() {
		t.Fatalf("generated %d routes, loader has %d", len(generated), loader.Len())
	}
	for _, route := range generated {
		loaded, ok := loader.Fetch(route.Name)
		if !ok {
			t.Errorf("generated route %q absent from loader", route.Name)
			continue
		}
		if route.IntentLabel != loaded.IntentLabel ||
			route.RouteGroup != loaded.RouteGroup ||
			route.HandlerKey != loaded.HandlerKey ||
			route.RequiredCitation != loaded.RequiredCitation ||
			route.VerificationStatus != loaded.VerificationStatus ||
			route.FieldRefsVerified != loaded.FieldRefsVerified ||
			route.Provenance != loaded.Provenance ||
			route.Path != loaded.Path {
			t.Errorf("route %q scalar drift: gen=%+v disk=%+v", route.Name, route, loaded)
		}
		if !equalStrings(route.RequiredTools, loaded.RequiredTools) ||
			!equalStrings(route.ToolSubset, loaded.ToolSubset) ||
			!equalStrings(route.PlannerDirectives, loaded.PlannerDirectives) {
			t.Errorf("route %q list drift: gen=%+v disk=%+v", route.Name, route, loaded)
		}
		if len(route.PlannerExamples) != len(loaded.PlannerExamples) {
			t.Errorf("route %q planner_examples len drift: gen=%d disk=%d", route.Name, len(route.PlannerExamples), len(loaded.PlannerExamples))
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
