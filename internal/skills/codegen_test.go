package skills

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

const generatedFile = "registry_gen.go"

// normalizeLF mirrors computeRegistryDigest's newline normalization so byte
// comparisons are CRLF/LF agnostic.
func normalizeLF(b []byte) []byte {
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(b, []byte("\r"), []byte("\n"))
}

// TestGeneratedRegistry_MatchesDisk is the codegen drift gate: regenerating from
// disk must reproduce the committed registry_gen.go byte-for-byte (after LF
// normalization). Mirrors the CI `go generate && git diff --exit-code` check
// in-process so a stale registry fails `go test ./...`.
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
		t.Fatalf("%s is stale — run `go generate ./internal/skills` and commit the result", generatedFile)
	}
}

// TestGeneratedRegistry_DigestPinned verifies registry_gen.go matches the pinned
// digest. Stronger than the drift gate: it pins a known-good snapshot. On a
// legitimate skill change, regenerate, then paste the printed digest into
// registry_digest.go.
func TestGeneratedRegistry_DigestPinned(t *testing.T) {
	src, err := os.ReadFile(generatedFile)
	if err != nil {
		t.Fatalf("read %s: %v", generatedFile, err)
	}
	got := computeRegistryDigest(src)
	if got != generatedRegistryDigestExpected {
		t.Fatalf("registry digest mismatch:\n  got  %s\n  want %s\n(update generatedRegistryDigestExpected after an intentional skill change)", got, generatedRegistryDigestExpected)
	}
}

// TestGeneratedRegistry_SemanticParityWithLoader gives generatedSkills a
// consumer and asserts the codegen output stays semantically in sync with the
// on-disk loader (same names, same required_tools).
func TestGeneratedRegistry_SemanticParityWithLoader(t *testing.T) {
	l, err := NewLoaderWithLogger(seededRoot, silentLogger())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	gen := GeneratedSkills()
	if len(gen) != l.Len() {
		t.Fatalf("generated %d skills, loader has %d", len(gen), l.Len())
	}
	for _, g := range gen {
		ls, ok := l.Fetch(g.Name)
		if !ok {
			t.Errorf("generated skill %q absent from loader", g.Name)
			continue
		}
		if !equalStrings(g.RequiredTools, ls.RequiredTools) {
			t.Errorf("skill %q required_tools drift: gen=%v disk=%v", g.Name, g.RequiredTools, ls.RequiredTools)
		}
		if g.License != ls.License || g.Compatibility != ls.Compatibility || g.AllowedTools != ls.AllowedTools {
			t.Errorf("skill %q standard metadata drift", g.Name)
		}
		if g.VerificationStatus != ls.VerificationStatus || g.FieldRefsVerified != ls.FieldRefsVerified {
			t.Errorf("skill %q verification drift: gen=%s/%v disk=%s/%v",
				g.Name, g.VerificationStatus, g.FieldRefsVerified, ls.VerificationStatus, ls.FieldRefsVerified)
		}
		if g.HandlerKey != ls.HandlerKey || g.IntentLabel != ls.IntentLabel || g.SkillGroup != ls.SkillGroup {
			t.Errorf("skill %q routing drift: gen=(%q,%q,%q) disk=(%q,%q,%q)",
				g.Name, g.HandlerKey, g.IntentLabel, g.SkillGroup, ls.HandlerKey, ls.IntentLabel, ls.SkillGroup)
		}
		if g.Provenance != ls.Provenance || g.SkillVersion != ls.SkillVersion || g.LastValidatedAgainst != ls.LastValidatedAgainst {
			t.Errorf("skill %q provenance drift", g.Name)
		}
		if !equalStrings(g.Resources.Scripts, ls.Resources.Scripts) ||
			!equalStrings(g.Resources.References, ls.Resources.References) ||
			!equalStrings(g.Resources.Tests, ls.Resources.Tests) {
			t.Errorf("skill %q resource drift: gen=%+v disk=%+v", g.Name, g.Resources, ls.Resources)
		}
		if g.Path != ls.Path {
			t.Errorf("skill %q path drift: gen=%q disk=%q", g.Name, g.Path, ls.Path)
		}
	}
}

func TestGenerateRegistry_RejectsDistilledSkillWithoutGovernanceGate(t *testing.T) {
	root := t.TempDir()
	writeCanonicalSkill(t, root, "distilled-candidate",
		"name: distilled-candidate\n"+
			"description: distilled candidate\n"+
			"metadata:\n"+
			"  verification_status: spike_validated\n"+
			"  field_refs_verified: true\n"+
			"  provenance: distilled_from_trajectory\n"+
			"  provenance_trace_ref: trace-abc\n"+
			"  skill_version: 2",
		"body\n")

	_, err := GenerateRegistry(root)
	if err == nil || !strings.Contains(err.Error(), "registration gate") {
		t.Fatalf("expected registration gate error, got %v", err)
	}
}

func TestGenerateRegistry_RejectsDeterministicRoutingMetadata(t *testing.T) {
	root := t.TempDir()
	writeCanonicalSkill(t, root, "route-shaped",
		"name: route-shaped\n"+
			"description: route shaped\n"+
			"metadata:\n"+
			"  verification_status: production_validated\n"+
			"  field_refs_verified: true\n"+
			"  intent_label: route-shaped\n"+
			"  handler_key: handleGPUSpecsQuery\n"+
			"  react_tool_subset:\n"+
			"    - DescribeAvailableCompShareInstanceTypes",
		"body\n")

	_, err := GenerateRegistry(root)
	if err == nil || !strings.Contains(err.Error(), "deterministic routing metadata") {
		t.Fatalf("expected deterministic routing metadata rejection, got %v", err)
	}
}

func TestGenerateRegistry_AllowsReviewedSanitizedDistilledSkill(t *testing.T) {
	root := t.TempDir()
	writeCanonicalSkill(t, root, "distilled-candidate",
		"name: distilled-candidate\n"+
			"description: distilled candidate\n"+
			"metadata:\n"+
			"  verification_status: spike_validated\n"+
			"  field_refs_verified: true\n"+
			"  provenance: distilled_from_trajectory\n"+
			"  provenance_trace_ref: trace-abc\n"+
			"  skill_version: 2\n"+
			"  last_validated_against: eval-sha256-abc\n"+
			"  human_reviewed: true\n"+
			"  sanitized: true\n"+
			"  eval_passed: true",
		"body\n")

	src, err := GenerateRegistry(root)
	if err != nil {
		t.Fatalf("GenerateRegistry: %v", err)
	}
	if !bytes.Contains(src, []byte(`"distilled-candidate"`)) {
		t.Fatalf("generated registry did not include distilled candidate:\n%s", src)
	}
}

func TestGenerateRegistry_RejectsHardcodedIdentifiersInDistilledSkill(t *testing.T) {
	root := t.TempDir()
	writeCanonicalSkill(t, root, "distilled-candidate",
		"name: distilled-candidate\n"+
			"description: distilled candidate\n"+
			"metadata:\n"+
			"  verification_status: spike_validated\n"+
			"  field_refs_verified: true\n"+
			"  provenance: distilled_from_trajectory\n"+
			"  provenance_trace_ref: trace-abc\n"+
			"  skill_version: 2\n"+
			"  last_validated_against: eval-sha256-abc\n"+
			"  human_reviewed: true\n"+
			"  sanitized: true\n"+
			"  eval_passed: true",
		"do not bake uhost-1qx1qsw4b1pk into a reusable skill\n")

	_, err := GenerateRegistry(root)
	if err == nil || !strings.Contains(err.Error(), "hard-coded identifier") {
		t.Fatalf("expected hard-coded identifier rejection, got %v", err)
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
