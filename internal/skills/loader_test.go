package skills

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// silentLogger discards the dangling-related_skills warnings so they don't spam
// test output. Tests that assert on the warning use a *bytes.Buffer logger.
func silentLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// writeSkill writes a skill bundle into root/<name>/skill.md. frontmatter is the
// YAML between the `---` fences (no fences); body is the markdown after.
func writeSkill(t *testing.T, root, name, frontmatter, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	content := "---\n" + frontmatter + "\n---\n\n" + body
	if err := os.WriteFile(filepath.Join(dir, "skill.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write skill.md: %v", err)
	}
}

// seededRoot is the package directory itself; the test binary runs with cwd set
// to internal/skills, so the 5 diagnose_* bundles live under ".".
const seededRoot = "."

// TestNewLoader_LoadsAllSeededSkills checks every on-disk skill loads and
// name==dir holds for all of them (load would fail otherwise). The set is the 5
// seeded diagnose_* playbooks (P1) plus the 6 migrated catalog capabilities (P2).
func TestNewLoader_LoadsAllSeededSkills(t *testing.T) {
	l, err := NewLoaderWithLogger(seededRoot, silentLogger())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	want := []string{
		"community_image_list",
		"custom_image_list",
		"diagnose_gpu_not_detected",
		"diagnose_image_issue",
		"diagnose_init_failure",
		"diagnose_port_firewall",
		"diagnose_ssh",
		"gpu_specs_query",
		"platform_image_list",
		"pricing_query",
		"stock_availability",
	}
	if l.Len() != len(want) {
		t.Fatalf("loaded %d skills, want %d (%v)", l.Len(), len(want), l.Names())
	}
	for _, name := range want {
		if _, ok := l.Fetch(name); !ok {
			t.Errorf("skill %q not loaded; got %v", name, l.Names())
		}
	}
}

// TestNewLoader_CapabilitySkillsCarryRoutingBlock asserts the 6 migrated
// capabilities carry the §3 routing block (intent_label == name, a handler_key,
// a non-empty react_tool_subset) and are production_validated — the fields the
// P2 intent bridge depends on.
func TestNewLoader_CapabilitySkillsCarryRoutingBlock(t *testing.T) {
	l, err := NewLoaderWithLogger(seededRoot, silentLogger())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	caps := map[string]string{
		"gpu_specs_query":      "handleGPUSpecsQuery",
		"stock_availability":   "handleStockAvailability",
		"platform_image_list":  "handlePlatformImageList",
		"custom_image_list":    "handleCustomImageList",
		"community_image_list": "handleCommunityImageList",
		"pricing_query":        "handlePricingQuery",
	}
	for name, wantHandler := range caps {
		s, ok := l.Fetch(name)
		if !ok {
			t.Errorf("capability skill %q not loaded", name)
			continue
		}
		if s.IntentLabel != name {
			t.Errorf("%s: intent_label = %q, want %q", name, s.IntentLabel, name)
		}
		if s.HandlerKey != wantHandler {
			t.Errorf("%s: handler_key = %q, want %q", name, s.HandlerKey, wantHandler)
		}
		if len(s.ReactToolSubset) == 0 {
			t.Errorf("%s: react_tool_subset is empty", name)
		}
		if s.VerificationStatus != VerificationProductionValidated {
			t.Errorf("%s: verification_status = %q, want production_validated", name, s.VerificationStatus)
		}
	}
}

// TestSkillBody_LazyCautionInjection asserts the single-choke-point dual caution
// for an unverified + field_refs:false skill, and that the frontmatter is
// stripped from the returned body.
func TestSkillBody_LazyCautionInjection(t *testing.T) {
	l, err := NewLoaderWithLogger(seededRoot, silentLogger())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	s, ok := l.Fetch("diagnose_ssh")
	if !ok {
		t.Fatal("diagnose_ssh not loaded")
	}
	body, err := s.Body()
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if !strings.HasPrefix(body, CautionUnverified) {
		t.Errorf("unverified skill body should be prefixed with caution; got prefix %q", head(body))
	}
	if !strings.HasSuffix(strings.TrimRight(body, "\n"), CautionFieldRefs) {
		t.Errorf("field_refs:false skill body should be suffixed with field-refs caution; got tail %q", tail(body))
	}
	if !strings.Contains(body, "# Diagnose: SSH Connection Failure") {
		t.Error("body should contain the authored markdown heading")
	}
	if strings.Contains(body, "verification_status") {
		t.Error("frontmatter leaked into body (verification_status present)")
	}
}

// TestGeneratedSkillBody_CWDIndependent proves the go:embed fix closed the CWD
// trap: a skill from the generated/runtime registry (bodyFS nil → package embed
// FS) resolves Body() even when the process CWD is NOT internal/skills — exactly
// the failure mode a deployed binary (B8 agent tier) would have hit with the old
// os.ReadFile(relative-path) implementation. Not parallel (mutates CWD).
func TestGeneratedSkillBody_CWDIndependent(t *testing.T) {
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// defer (not t.Cleanup) so CWD is restored before t.TempDir's RemoveAll
	// cleanup runs — on Windows a directory that is the process CWD cannot be
	// unlinked.
	defer func() { _ = os.Chdir(orig) }()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	var ssh *Skill
	for _, s := range GeneratedSkills() {
		if s.Name == "diagnose_ssh" {
			ssh = s
			break
		}
	}
	if ssh == nil {
		t.Fatal("diagnose_ssh missing from generated registry")
	}
	if ssh.bodyFS != nil {
		t.Fatalf("generated skill must have nil bodyFS (embed-backed); got %T", ssh.bodyFS)
	}
	body, err := ssh.Body()
	if err != nil {
		t.Fatalf("Body() from a non-package CWD must succeed (embed-backed): %v", err)
	}
	if !strings.HasPrefix(body, CautionUnverified) {
		t.Errorf("embed-backed body should still inject caution; got prefix %q", head(body))
	}
	if !strings.Contains(body, "# Diagnose: SSH Connection Failure") {
		t.Error("embed-backed body should contain the authored markdown heading")
	}
}

// TestSkillBody_OverCapFailsNotTruncate: an over-cap body fails at Body() (lazy)
// with an error, never a silent truncation. NewLoader still succeeds (body
// unread).
func TestSkillBody_OverCapFailsNotTruncate(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "over_cap",
		"name: over_cap\ndescription: cap test\nverification_status: production_validated\nfield_refs_verified: true\nbody_cap_lines: 5",
		strings.Repeat("a body line\n", 10))
	l, err := NewLoaderWithLogger(root, silentLogger())
	if err != nil {
		t.Fatalf("NewLoader should not fail (body unread): %v", err)
	}
	s, _ := l.Fetch("over_cap")
	body, err := s.Body()
	if err == nil {
		t.Fatalf("Body should fail for over-cap skill; got body %q", body)
	}
	if !strings.Contains(err.Error(), "exceeds cap") {
		t.Errorf("error should mention the cap; got %v", err)
	}
	if body != "" {
		t.Errorf("over-cap Body should return empty string, not a truncation; got %q", body)
	}
}

// TestNewLoader_BodyCapCeilingRejected: body_cap_lines above the hard ceiling
// fails at load, so a skill can't disable the cap with body_cap_lines: 1000.
func TestNewLoader_BodyCapCeilingRejected(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "huge_cap",
		"name: huge_cap\ndescription: x\nverification_status: production_validated\nfield_refs_verified: true\nbody_cap_lines: 201",
		"body\n")
	if _, err := NewLoaderWithLogger(root, silentLogger()); err == nil || !strings.Contains(err.Error(), "hard ceiling") {
		t.Fatalf("expected hard-ceiling error, got %v", err)
	}
}

// TestNewLoader_StrictVerificationStatus: missing or unknown verification_status
// fails (no permissive default, ADR-004 §88).
func TestNewLoader_StrictVerificationStatus(t *testing.T) {
	// Frontmatter tails (name line is prepended per-case to keep name==dir).
	cases := map[string]string{
		"missing": "description: x\nfield_refs_verified: true",
		"unknown": "description: x\nverification_status: bogus\nfield_refs_verified: true",
	}
	for label, fmTail := range cases {
		t.Run(label, func(t *testing.T) {
			root := t.TempDir()
			name := "vs_" + label
			writeSkill(t, root, name, "name: "+name+"\n"+fmTail, "body\n")
			if _, err := NewLoaderWithLogger(root, silentLogger()); err == nil || !strings.Contains(err.Error(), "verification_status") {
				t.Fatalf("expected verification_status error, got %v", err)
			}
		})
	}
}

// TestNewLoader_FieldRefsVerifiedRequired: an omitted field_refs_verified fails
// (the *bool distinguishes absent from explicit false).
func TestNewLoader_FieldRefsVerifiedRequired(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "fr_missing",
		"name: fr_missing\ndescription: x\nverification_status: production_validated",
		"body\n")
	if _, err := NewLoaderWithLogger(root, silentLogger()); err == nil || !strings.Contains(err.Error(), "field_refs_verified") {
		t.Fatalf("expected field_refs_verified error, got %v", err)
	}
}

// TestNewLoader_RejectsUnknownYAMLKey guards KnownFields(true): an unknown key
// is a hard parse failure (so a P2 schema-key typo can't silently no-op).
func TestNewLoader_RejectsUnknownYAMLKey(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "unknown_key",
		"name: unknown_key\ndescription: x\nverification_status: production_validated\nfield_refs_verified: true\nnot_a_real_field: 1",
		"body\n")
	if _, err := NewLoaderWithLogger(root, silentLogger()); err == nil || !strings.Contains(err.Error(), "not_a_real_field") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

// TestNewLoader_NameMustMatchDir: a name field that disagrees with the directory
// name fails (ADR-004 §66 — prevents directory/name drift).
func TestNewLoader_NameMustMatchDir(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "dir_name",
		"name: other_name\ndescription: x\nverification_status: production_validated\nfield_refs_verified: true",
		"body\n")
	if _, err := NewLoaderWithLogger(root, silentLogger()); err == nil || !strings.Contains(err.Error(), "directory name") {
		t.Fatalf("expected name!=dir error, got %v", err)
	}
}

// TestNewLoader_DanglingRelatedSkillsWarnsNotFails: the seeded skills reference
// safety_warning (not authored yet). That is a forward reference — warn, never
// fail.
func TestNewLoader_DanglingRelatedSkillsWarnsNotFails(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	if _, err := NewLoaderWithLogger(seededRoot, logger); err != nil {
		t.Fatalf("dangling related_skills must not fail the load: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "dangling forward reference") || !strings.Contains(out, "safety_warning") {
		t.Errorf("expected a dangling-related_skills warning mentioning safety_warning; got %q", out)
	}
}

// TestSkillBody_ConcurrentFetchRace exercises the sync.Once body cache under
// concurrency. Run with -race (internal/skills is on a -race CI target).
func TestSkillBody_ConcurrentFetchRace(t *testing.T) {
	l, err := NewLoaderWithLogger(seededRoot, silentLogger())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	s, _ := l.Fetch("diagnose_ssh")
	const n = 24
	var wg sync.WaitGroup
	results := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body, err := s.Body()
			if err != nil {
				t.Errorf("Body: %v", err)
				return
			}
			results[i] = body
		}(i)
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if results[i] != results[0] {
			t.Fatalf("concurrent Body returned different values at %d", i)
		}
	}
}

// TestLoaderCautionIgnoresEvolutionFields is the ADR-008 §B half-wired guard:
// the caution logic reads ONLY verification_status / field_refs_verified. A
// production_validated + field_refs:true skill must get NO caution lines even
// when every evolution field is populated to a non-default value; flipping only
// verification_status to unverified must reintroduce the caution. This proves the
// 4 reserved evolution fields are forward-declared schema with no loader branch.
func TestLoaderCautionIgnoresEvolutionFields(t *testing.T) {
	evolution := "provenance: distilled_from_trajectory\nprovenance_trace_ref: trace-xyz\nskill_version: 7\nlast_validated_against: snap-abc"

	root := t.TempDir()
	writeSkill(t, root, "evo_clean",
		"name: evo_clean\ndescription: x\nverification_status: production_validated\nfield_refs_verified: true\n"+evolution,
		"clean body\n")
	l, err := NewLoaderWithLogger(root, silentLogger())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	s, _ := l.Fetch("evo_clean")
	body, err := s.Body()
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if strings.Contains(body, CautionUnverified) || strings.Contains(body, CautionFieldRefs) {
		t.Fatalf("evolution fields must not trigger caution; got body %q", body)
	}

	// Same evolution fields, only verification_status flipped → caution returns,
	// proving the driver is verification_status, not the evolution metadata.
	root2 := t.TempDir()
	writeSkill(t, root2, "evo_unverified",
		"name: evo_unverified\ndescription: x\nverification_status: unverified\nfield_refs_verified: true\n"+evolution,
		"unverified body\n")
	l2, err := NewLoaderWithLogger(root2, silentLogger())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	s2, _ := l2.Fetch("evo_unverified")
	body2, err := s2.Body()
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if !strings.HasPrefix(body2, CautionUnverified) {
		t.Errorf("verification_status:unverified must inject caution regardless of evolution fields; got %q", head(body2))
	}
}

// TestSeededSkills_DeclareProvenance is the ADR-008 CI existence check: every
// seeded skill explicitly declares provenance (no default), mirroring the
// verification_status discipline (ADR-004 §88).
func TestSeededSkills_DeclareProvenance(t *testing.T) {
	l, err := NewLoaderWithLogger(seededRoot, silentLogger())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	for _, name := range l.Names() {
		s, _ := l.Fetch(name)
		if s.Provenance != "human_authored" {
			t.Errorf("skill %q provenance = %q, want human_authored (explicit, no default)", name, s.Provenance)
		}
	}
}

// TestNewLoader_ToleratesAbsentEvolutionFields: a minimal skill with only the
// required fields loads, and the loader does NOT default the reserved evolution
// fields (empty-value semantics are the future consumer's concern, not the
// loader's — it stays zero-branch).
func TestNewLoader_ToleratesAbsentEvolutionFields(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "minimal",
		"name: minimal\ndescription: minimal skill\nverification_status: production_validated\nfield_refs_verified: true",
		"minimal body\n")
	l, err := NewLoaderWithLogger(root, silentLogger())
	if err != nil {
		t.Fatalf("minimal skill should load: %v", err)
	}
	s, _ := l.Fetch("minimal")
	if s.Provenance != "" || s.SkillVersion != 0 || s.LastValidatedAgainst != "" {
		t.Errorf("loader must not default evolution fields; got provenance=%q version=%d last=%q",
			s.Provenance, s.SkillVersion, s.LastValidatedAgainst)
	}
	if _, err := s.Body(); err != nil {
		t.Errorf("Body of minimal skill: %v", err)
	}
}

func head(s string) string {
	if len(s) > 60 {
		return s[:60]
	}
	return s
}

func tail(s string) string {
	s = strings.TrimRight(s, "\n")
	if len(s) > 60 {
		return s[len(s)-60:]
	}
	return s
}
