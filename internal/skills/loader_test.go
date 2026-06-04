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

// writeSkill writes a canonical test skill bundle into root/<name>/SKILL.md.
// Tests may pass legacy flat frontmatter for brevity; the helper folds every
// non-standard key under metadata so fixtures exercise the R1 shape.
func writeSkill(t *testing.T, root, name, frontmatter, body string) {
	t.Helper()
	writeCanonicalSkill(t, root, name, canonicalTestFrontmatter(frontmatter), body)
}

func canonicalTestFrontmatter(frontmatter string) string {
	if strings.Contains(frontmatter, "\nmetadata:") || strings.HasPrefix(frontmatter, "metadata:") {
		return frontmatter
	}
	var top []string
	var meta []string
	for _, line := range strings.Split(frontmatter, "\n") {
		switch {
		case strings.HasPrefix(line, "name:") || strings.HasPrefix(line, "description:"):
			top = append(top, line)
		case strings.TrimSpace(line) != "":
			meta = append(meta, "  "+line)
		}
	}
	if len(meta) > 0 {
		top = append(top, "metadata:")
		top = append(top, meta...)
	}
	return strings.Join(top, "\n")
}

func writeCanonicalSkill(t *testing.T, root, name, frontmatter, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	content := "---\n" + frontmatter + "\n---\n\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
}

// seededRoot is the package directory itself; the test binary runs with cwd set
// to internal/skills, so the 5 diagnose-* bundles live under ".".
const seededRoot = "."

func TestNewLoader_LoadsCanonicalSKILLMDWithNestedMetadata(t *testing.T) {
	root := t.TempDir()
	writeCanonicalSkill(t, root, "canonical-skill",
		"name: canonical-skill\n"+
			"description: canonical skill\n"+
			"license: UNLICENSED\n"+
			"compatibility: CompShare diagnosis executor; read-only platform APIs only.\n"+
			"allowed-tools: DescribeCompShareInstance\n"+
			"metadata:\n"+
			"  verification_status: production_validated\n"+
			"  field_refs_verified: true\n"+
			"  required_tools:\n"+
			"    - DescribeCompShareInstance\n"+
			"  applicable_tiers: [agent]",
		"body\n")

	l, err := NewLoaderWithLogger(root, silentLogger())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	s, ok := l.Fetch("canonical-skill")
	if !ok {
		t.Fatalf("canonical-skill not loaded; got %v", l.Names())
	}
	if !strings.HasSuffix(s.Path, "canonical-skill/SKILL.md") {
		t.Fatalf("Path = %q, want canonical SKILL.md path", s.Path)
	}
	if s.License != "UNLICENSED" {
		t.Fatalf("License = %q", s.License)
	}
	if s.Compatibility == "" {
		t.Fatal("Compatibility should load from standard frontmatter")
	}
	if s.AllowedTools != "DescribeCompShareInstance" {
		t.Fatalf("AllowedTools = %q", s.AllowedTools)
	}
	if !equalStrings(s.RequiredTools, []string{"DescribeCompShareInstance"}) {
		t.Fatalf("required_tools = %v", s.RequiredTools)
	}
	body, err := s.Body()
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if body != "body\n" {
		t.Fatalf("Body = %q", body)
	}
}

func TestNewLoader_RejectsUnderscoreSkillNames(t *testing.T) {
	root := t.TempDir()
	writeCanonicalSkill(t, root, "bad_name",
		"name: bad_name\n"+
			"description: bad name\n"+
			"metadata:\n"+
			"  verification_status: production_validated\n"+
			"  field_refs_verified: true",
		"body\n")
	if _, err := NewLoaderWithLogger(root, silentLogger()); err == nil || !strings.Contains(err.Error(), "lowercase letters, digits, and hyphens") {
		t.Fatalf("expected Anthropic-style name error, got %v", err)
	}
}

func TestNewLoader_AllowedToolsIsAdvisoryOnly(t *testing.T) {
	root := t.TempDir()
	writeCanonicalSkill(t, root, "advisory-tools",
		"name: advisory-tools\n"+
			"description: advisory tools\n"+
			"allowed-tools: CreateCompShareCustomImage\n"+
			"metadata:\n"+
			"  verification_status: production_validated\n"+
			"  field_refs_verified: true\n"+
			"  required_tools:\n"+
			"    - DescribeCompShareInstance",
		"body\n")
	l, err := NewLoaderWithLogger(root, silentLogger())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	s, ok := l.Fetch("advisory-tools")
	if !ok {
		t.Fatalf("advisory-tools not loaded; got %v", l.Names())
	}
	if s.AllowedTools != "CreateCompShareCustomImage" {
		t.Fatalf("AllowedTools = %q", s.AllowedTools)
	}
	if !equalStrings(s.RequiredTools, []string{"DescribeCompShareInstance"}) {
		t.Fatalf("required_tools must remain the executable tool set; got %v", s.RequiredTools)
	}
}

func TestNewLoader_RejectsLegacyTopLevelOperationalFieldsInSKILLMD(t *testing.T) {
	root := t.TempDir()
	writeCanonicalSkill(t, root, "legacy-shape",
		"name: legacy-shape\n"+
			"description: legacy shape\n"+
			"verification_status: production_validated\n"+
			"field_refs_verified: true",
		"body\n")

	_, err := NewLoaderWithLogger(root, silentLogger())
	if err == nil || !strings.Contains(err.Error(), "verification_status") {
		t.Fatalf("expected top-level verification_status to be rejected, got %v", err)
	}
}

func TestNewLoader_ListsProgressiveDisclosureResources(t *testing.T) {
	root := t.TempDir()
	name := "resourceful-skill"
	writeCanonicalSkill(t, root, name,
		"name: resourceful-skill\n"+
			"description: resourceful skill\n"+
			"metadata:\n"+
			"  verification_status: production_validated\n"+
			"  field_refs_verified: true",
		"body\n")
	for _, rel := range []string{
		filepath.Join("scripts", "check.ps1"),
		filepath.Join("references", "api.md"),
		filepath.Join("tests", "case.json"),
	} {
		path := filepath.Join(root, name, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir resource dir: %v", err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("write resource %s: %v", rel, err)
		}
	}

	l, err := NewLoaderWithLogger(root, silentLogger())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	s, _ := l.Fetch(name)
	if !equalStrings(s.Resources.Scripts, []string{"scripts/check.ps1"}) {
		t.Fatalf("scripts = %v", s.Resources.Scripts)
	}
	if !equalStrings(s.Resources.References, []string{"references/api.md"}) {
		t.Fatalf("references = %v", s.Resources.References)
	}
	if !equalStrings(s.Resources.Tests, []string{"tests/case.json"}) {
		t.Fatalf("tests = %v", s.Resources.Tests)
	}
}

// TestNewLoader_LoadsAllSeededSkills checks every on-disk true skill loads and
// name==dir holds for all of them (load would fail otherwise). Deterministic
// routes live under internal/routing and saga workflow arms are not body-read
// skills.
func TestNewLoader_LoadsAllSeededSkills(t *testing.T) {
	l, err := NewLoaderWithLogger(seededRoot, silentLogger())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	want := []string{
		"diagnose-gpu-not-detected",
		"diagnose-image-issue",
		"diagnose-init-failure",
		"diagnose-port-firewall",
		"diagnose-ssh",
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

// TestNewLoader_TrueSkillsDoNotCarryDeterministicRoutingBlocks asserts the true
// skill registry is reserved for body-read playbooks. Deterministic route
// metadata belongs under internal/routing.
func TestNewLoader_TrueSkillsDoNotCarryDeterministicRoutingBlocks(t *testing.T) {
	l, err := NewLoaderWithLogger(seededRoot, silentLogger())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	for _, name := range l.Names() {
		s, ok := l.Fetch(name)
		if !ok {
			t.Fatalf("loaded name %q could not be fetched", name)
		}
		if s.IntentLabel != "" || s.HandlerKey != "" || len(s.ReactToolSubset) > 0 {
			t.Errorf("true skill %q carries deterministic routing metadata: intent=%q handler=%q subset=%v",
				name, s.IntentLabel, s.HandlerKey, s.ReactToolSubset)
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
	s, ok := l.Fetch("diagnose-ssh")
	if !ok {
		t.Fatal("diagnose-ssh not loaded")
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
		if s.Name == "diagnose-ssh" {
			ssh = s
			break
		}
	}
	if ssh == nil {
		t.Fatal("diagnose-ssh missing from generated registry")
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
	writeSkill(t, root, "over-cap",
		"name: over-cap\ndescription: cap test\nverification_status: production_validated\nfield_refs_verified: true\nbody_cap_lines: 5",
		strings.Repeat("a body line\n", 10))
	l, err := NewLoaderWithLogger(root, silentLogger())
	if err != nil {
		t.Fatalf("NewLoader should not fail (body unread): %v", err)
	}
	s, _ := l.Fetch("over-cap")
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
	writeSkill(t, root, "huge-cap",
		"name: huge-cap\ndescription: x\nverification_status: production_validated\nfield_refs_verified: true\nbody_cap_lines: 201",
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
			name := "vs-" + label
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
	writeSkill(t, root, "fr-missing",
		"name: fr-missing\ndescription: x\nverification_status: production_validated",
		"body\n")
	if _, err := NewLoaderWithLogger(root, silentLogger()); err == nil || !strings.Contains(err.Error(), "field_refs_verified") {
		t.Fatalf("expected field_refs_verified error, got %v", err)
	}
}

// TestNewLoader_RejectsUnknownApplicableTier closes the applicable_tiers enum
// (ADR-001 two-lane model): a typo like "fas" fails to load rather than silently
// routing the skill to no lane. P3a-1 fast-tier determinism guard.
func TestNewLoader_RejectsUnknownApplicableTier(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "bad-tier",
		"name: bad-tier\ndescription: x\nverification_status: production_validated\nfield_refs_verified: true\napplicable_tiers: [fas]",
		"body\n")
	if _, err := NewLoaderWithLogger(root, silentLogger()); err == nil || !strings.Contains(err.Error(), "applicable_tiers") {
		t.Fatalf("expected applicable_tiers enum error, got %v", err)
	}
}

// TestNewLoader_AcceptsKnownApplicableTiers confirms the enum check does not
// reject the two valid lanes (non-vacuity guard for the test above).
func TestNewLoader_AcceptsKnownApplicableTiers(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "good-tier",
		"name: good-tier\ndescription: x\nverification_status: production_validated\nfield_refs_verified: true\napplicable_tiers: [fast, agent]",
		"body\n")
	if _, err := NewLoaderWithLogger(root, silentLogger()); err != nil {
		t.Fatalf("fast+agent are valid tiers, load must succeed: %v", err)
	}
}

// TestNewLoader_RejectsUnknownYAMLKey guards KnownFields(true): an unknown key
// is a hard parse failure (so a P2 schema-key typo can't silently no-op).
func TestNewLoader_RejectsUnknownYAMLKey(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "unknown-key",
		"name: unknown-key\ndescription: x\nverification_status: production_validated\nfield_refs_verified: true\nnot_a_real_field: 1",
		"body\n")
	if _, err := NewLoaderWithLogger(root, silentLogger()); err == nil || !strings.Contains(err.Error(), "not_a_real_field") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

// TestNewLoader_NameMustMatchDir: a name field that disagrees with the directory
// name fails (ADR-004 §66 — prevents directory/name drift).
func TestNewLoader_NameMustMatchDir(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "dir-name",
		"name: other-name\ndescription: x\nverification_status: production_validated\nfield_refs_verified: true",
		"body\n")
	if _, err := NewLoaderWithLogger(root, silentLogger()); err == nil || !strings.Contains(err.Error(), "directory name") {
		t.Fatalf("expected name!=dir error, got %v", err)
	}
}

// TestNewLoader_DanglingRelatedSkillsWarnsNotFails: forward references are
// warned, never failed.
func TestNewLoader_DanglingRelatedSkillsWarnsNotFails(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "has-forward-ref",
		"name: has-forward-ref\ndescription: x\nverification_status: production_validated\nfield_refs_verified: true\nrelated_skills: [future-safety-warning]",
		"body\n")
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	if _, err := NewLoaderWithLogger(root, logger); err != nil {
		t.Fatalf("dangling related_skills must not fail the load: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "dangling forward reference") || !strings.Contains(out, "future-safety-warning") {
		t.Errorf("expected a dangling-related_skills warning mentioning future-safety-warning; got %q", out)
	}
}

// TestSkillBody_ConcurrentFetchRace exercises the sync.Once body cache under
// concurrency. Run with -race (internal/skills is on a -race CI target).
func TestSkillBody_ConcurrentFetchRace(t *testing.T) {
	l, err := NewLoaderWithLogger(seededRoot, silentLogger())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	s, _ := l.Fetch("diagnose-ssh")
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
	writeSkill(t, root, "evo-clean",
		"name: evo-clean\ndescription: x\nverification_status: production_validated\nfield_refs_verified: true\n"+evolution,
		"clean body\n")
	l, err := NewLoaderWithLogger(root, silentLogger())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	s, _ := l.Fetch("evo-clean")
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
	writeSkill(t, root2, "evo-unverified",
		"name: evo-unverified\ndescription: x\nverification_status: unverified\nfield_refs_verified: true\n"+evolution,
		"unverified body\n")
	l2, err := NewLoaderWithLogger(root2, silentLogger())
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	s2, _ := l2.Fetch("evo-unverified")
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
// fields. Registration policy for those fields lives in codegen's
// ValidateSkillRegistration gate.
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
