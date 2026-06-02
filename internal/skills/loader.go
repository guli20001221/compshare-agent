// Package skills implements the ADR-004 skill bundle loader: one skill = one
// directory under internal/skills/<name>/SKILL.md, parsed as YAML frontmatter +
// markdown body with progressive disclosure (name/description eager, body lazy).
//
// Current scope: this package owns true body-read skills, currently the 5
// seeded diagnose_* skills. Deterministic read-only routes live under
// internal/routing/*/route.yaml and are not loaded as skills. The engine reads
// skill bodies only behind USE_SKILL_EXECUTOR plus the diagnosis allowlist.
//
// Contracts (ADR-004 + ADR-008 section B):
//   - Strict parse: verification_status + field_refs_verified are mandatory with
//     no permissive default; unknown YAML keys are rejected (KnownFields(true)).
//   - Body() is lazy (sync.Once) and is the single choke point that injects the
//     two caution lines and enforces body_cap_lines (fail, never truncate).
//   - Evolution/governance metadata is parsed and carried through the registry.
//     Body() never branches on it; GenerateRegistry interprets it only at the
//     registration gate for distilled/agent-created skills.
package skills

//go:generate go run github.com/compshare-agent/cmd/skillgen --root . --out registry_gen.go

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// skillFS embeds every internal/skills/<name>/SKILL.md so Body() reads are
// CWD-independent for the generated/runtime registry. The generated literals in
// registry_gen.go leave bodyFS nil and fall back to this embedded FS; a deployed
// binary (the agent tier) whose CWD is not internal/skills therefore still
// resolves skill bodies (closes the B2b P2 forward-risk in loadBody). The embed
// key is "<name>/SKILL.md"; name==dir is enforced at parse, so it is stable.
//
//go:embed */SKILL.md
var skillFS embed.FS

const (
	SkillFileName = "SKILL.md"

	// DefaultBodyCapLines is the body line cap applied when a skill omits
	// body_cap_lines. ADR-004 Body-Cap: 100 lines, stricter than Anthropic's
	// 500 because ds-v4-flash avalanches at 5K->11K input tokens.
	DefaultBodyCapLines = 100
	// MaxBodyCapLines is the hard ceiling on the per-skill body_cap_lines
	// override. A skill declaring more fails to load; this prevents a
	// body_cap_lines: 1000 from silently disabling the whole cap mechanism.
	MaxBodyCapLines = 200

	// TierFast and TierAgent are the ONLY permitted applicable_tiers values
	// (ADR-001 two-lane model). The enum is closed: a typo like "fas" fails to
	// load rather than silently routing a skill to no lane. fast = deterministic
	// route dispatch (no LLM); agent = body-driven executor / saga arm.
	TierFast  = "fast"
	TierAgent = "agent"

	// CautionUnverified is prepended to a body whose verification_status is
	// unverified (ADR-004 section 81). CautionFieldRefs is appended when
	// field_refs_verified is false (ADR-004 section 104). Body() is the only place
	// these are injected (single choke point).
	CautionUnverified = "[CAUTION: this methodology is unverified, treat steps as suggestions not facts]"
	CautionFieldRefs  = "[FIELD REFS NOT VERIFIED - confirm field names against actual API response before action]"
)

// verification_status enum (ADR-004 section 75).
const (
	VerificationProductionValidated = "production_validated"
	VerificationSpikeValidated      = "spike_validated"
	VerificationUnverified          = "unverified"
)

const (
	ProvenanceHumanAuthored           = "human_authored"
	ProvenanceDistilledFromTrajectory = "distilled_from_trajectory"
	ProvenanceAgentCreated            = "agent_created"
)

// skillNameRE enforces the ADR-004 name charset decision: snake_case, not the
// Anthropic kebab-case, for Go-package alignment.
var skillNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*[a-z0-9]$`)

// SkillPlannerExample is one Stage-2C routing example carried in the optional
// routing block (route dialect). Tagged for both YAML (on-disk) and the
// generated Go literal does not use the tags, but keeping them documents intent.
type SkillPlannerExample struct {
	Question   string  `yaml:"question"`
	Confidence float64 `yaml:"confidence"`
}

type SkillResources struct {
	Scripts    []string
	References []string
	Tests      []string
}

// Skill is one loaded skill bundle. Metadata fields are eager; the body is read
// lazily by Body(). The struct holds a sync.Once and therefore MUST NOT be
// copied; Loader and the generated registry always hand out *Skill.
type Skill struct {
	// ADR-004 core frontmatter (shared by both dialects).
	Name            string
	Description     string
	Triggers        []string
	ApplicableTiers []string
	RequiredTools   []string
	RelatedSkills   []string
	BodyCapLines    int // 0 means DefaultBodyCapLines; see effectiveBodyCap

	// Verification twin (ADR-004 section 88): mandatory, no permissive default.
	VerificationStatus string
	FieldRefsVerified  bool

	// Optional routing block (route dialect, B2b section 3). The 5 diagnose_*
	// skills omit all of these; that is valid; KnownFields(true) rejects only
	// unknown keys, not missing optional ones.
	IntentLabel       string
	SkillGroup        string
	RequiredCitation  bool
	HandlerKey        string
	ReactToolSubset   []string
	PlannerDirectives []string
	PlannerExamples   []SkillPlannerExample

	// Reserved evolution/governance metadata (ADR-008 section B). Body() treats
	// these as inert metadata; ValidateSkillRegistration interprets them only when
	// codegen admits distilled/agent-created skills into the generated registry.
	Provenance           string
	ProvenanceTraceRef   string
	SkillVersion         int
	LastValidatedAgainst string

	HumanReviewed bool
	Sanitized     bool
	EvalPassed    bool

	Resources SkillResources

	// Path is the slash-normalized location of SKILL.md (relative to the load
	// root). Slash-normalized so the generated registry is byte-identical across
	// Windows and Unix checkouts (B2b section 3 F4 determinism).
	Path string

	// bodyFS is the filesystem Body() reads from, keyed by "<name>/SKILL.md".
	// NewLoader sets it to os.DirFS(root) (disk, CWD-independent for the root it
	// was given). The generated registry leaves it nil; loadBody falls back to
	// the package-embedded skillFS. Never serialized by codegen (unexported).
	bodyFS   fs.FS
	bodyOnce sync.Once
	body     string
	bodyErr  error
}

// skillFrontmatter is the YAML decode target. field_refs_verified is a *bool so
// absence (nil) is distinguishable from an explicit false; both
// verification_status and field_refs_verified are mandatory with no default.
type skillFrontmatter struct {
	Name        string        `yaml:"name"`
	Description string        `yaml:"description"`
	Metadata    skillMetadata `yaml:"metadata"`
}

type skillMetadata struct {
	Triggers        []string `yaml:"triggers"`
	ApplicableTiers []string `yaml:"applicable_tiers"`
	RequiredTools   []string `yaml:"required_tools"`
	RelatedSkills   []string `yaml:"related_skills"`
	BodyCapLines    int      `yaml:"body_cap_lines"`

	VerificationStatus string `yaml:"verification_status"`
	FieldRefsVerified  *bool  `yaml:"field_refs_verified"`

	IntentLabel       string                `yaml:"intent_label"`
	SkillGroup        string                `yaml:"skill_group"`
	RequiredCitation  bool                  `yaml:"required_citation"`
	HandlerKey        string                `yaml:"handler_key"`
	ReactToolSubset   []string              `yaml:"react_tool_subset"`
	PlannerDirectives []string              `yaml:"planner_directives"`
	PlannerExamples   []SkillPlannerExample `yaml:"planner_examples"`

	Provenance           string `yaml:"provenance"`
	ProvenanceTraceRef   string `yaml:"provenance_trace_ref"`
	SkillVersion         int    `yaml:"skill_version"`
	LastValidatedAgainst string `yaml:"last_validated_against"`

	HumanReviewed bool `yaml:"human_reviewed"`
	Sanitized     bool `yaml:"sanitized"`
	EvalPassed    bool `yaml:"eval_passed"`
}

// SkillMeta is the eager, planner-visible projection (~80 tokens). P3 renders a
// catalog from these; the body is never included.
type SkillMeta struct {
	Name        string
	Description string
	Triggers    []string
}

// Loader holds the parsed skill bundles keyed by name.
type Loader struct {
	skills map[string]*Skill
}

// NewLoader scans root/*/SKILL.md, parses frontmatter only (bodies stay on disk
// until Body()), and validates each skill. Dangling related_skills references
// are warned (not failed) via log.Default(). Use NewLoaderWithLogger to capture
// warnings.
func NewLoader(root string) (*Loader, error) {
	return NewLoaderWithLogger(root, log.Default())
}

// NewLoaderWithLogger is NewLoader with an injectable logger for the
// dangling-related_skills warning (testability).
func NewLoaderWithLogger(root string, logger *log.Logger) (*Loader, error) {
	if logger == nil {
		logger = log.Default()
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("skills: read root %q: %w", root, err)
	}
	// rootFS roots Body() reads at the load root so they do not depend on the
	// process CWD (the generated registry instead reads the package embed FS).
	rootFS := os.DirFS(root)
	loaded := make(map[string]*Skill)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name(), SkillFileName)
		if _, statErr := os.Stat(path); statErr != nil {
			// A directory without SKILL.md is not a skill bundle (e.g. a future
			// shared-asset dir). Skip silently rather than failing the whole load.
			continue
		}
		s, parseErr := ParseSkillFile(path)
		if parseErr != nil {
			return nil, fmt.Errorf("skills: load %q: %w", path, parseErr)
		}
		if _, dup := loaded[s.Name]; dup {
			return nil, fmt.Errorf("skills: duplicate skill name %q", s.Name)
		}
		s.bodyFS = rootFS
		loaded[s.Name] = s
	}
	// Dangling related_skills are forward references (e.g. safety_warning is not
	// authored yet); warn, never fail (B2b section 4).
	for _, name := range sortedNames(loaded) {
		for _, rel := range loaded[name].RelatedSkills {
			if _, ok := loaded[rel]; !ok {
				logger.Printf("skills: skill %q references related_skill %q which is not loaded (dangling forward reference)", name, rel)
			}
		}
	}
	return &Loader{skills: loaded}, nil
}

// ParseSkillFile reads one SKILL.md, parses + validates its frontmatter, and
// returns a *Skill with the body unread. Exported so cmd/skillgen reuses the one
// parser (no second dialect that could drift from the loader).
func ParseSkillFile(path string) (*Skill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read skill file: %w", err)
	}
	frontmatter, _, err := splitFrontmatter(data)
	if err != nil {
		return nil, err
	}
	var raw skillFrontmatter
	dec := yaml.NewDecoder(bytes.NewReader([]byte(frontmatter)))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("yaml unmarshal: %w", err)
	}

	if raw.Name == "" {
		return nil, fmt.Errorf("name must be non-empty")
	}
	if len(raw.Name) > 64 || !skillNameRE.MatchString(raw.Name) {
		return nil, fmt.Errorf("name %q must match [a-z][a-z0-9_]*[a-z0-9] and be 1-64 chars", raw.Name)
	}
	if dir := filepath.Base(filepath.Dir(path)); raw.Name != dir {
		return nil, fmt.Errorf("name %q must equal directory name %q (ADR-004 section 66)", raw.Name, dir)
	}
	if strings.TrimSpace(raw.Description) == "" {
		return nil, fmt.Errorf("skill %q: description must be non-empty", raw.Name)
	}
	meta := raw.Metadata
	switch meta.VerificationStatus {
	case VerificationProductionValidated, VerificationSpikeValidated, VerificationUnverified:
	default:
		return nil, fmt.Errorf("skill %q: verification_status %q must be one of production_validated|spike_validated|unverified (no default permitted, ADR-004 section 88)", raw.Name, meta.VerificationStatus)
	}
	for _, tier := range meta.ApplicableTiers {
		if tier != TierFast && tier != TierAgent {
			return nil, fmt.Errorf("skill %q: applicable_tiers contains %q, must be one of %s|%s (ADR-001 two-lane model)", raw.Name, tier, TierFast, TierAgent)
		}
	}
	if meta.FieldRefsVerified == nil {
		return nil, fmt.Errorf("skill %q: field_refs_verified must be explicitly set (no default permitted, ADR-004 section 88)", raw.Name)
	}
	if meta.BodyCapLines < 0 {
		return nil, fmt.Errorf("skill %q: body_cap_lines must be >= 0", raw.Name)
	}
	if meta.BodyCapLines > MaxBodyCapLines {
		return nil, fmt.Errorf("skill %q: body_cap_lines %d exceeds hard ceiling %d (ADR-004 section 149)", raw.Name, meta.BodyCapLines, MaxBodyCapLines)
	}
	for i, ex := range meta.PlannerExamples {
		if strings.TrimSpace(ex.Question) == "" {
			return nil, fmt.Errorf("skill %q: planner_examples[%d].question must be non-empty", raw.Name, i)
		}
		if ex.Confidence < 0 || ex.Confidence > 1 {
			return nil, fmt.Errorf("skill %q: planner_examples[%d].confidence must be in [0,1]", raw.Name, i)
		}
	}

	return &Skill{
		Name:                 raw.Name,
		Description:          raw.Description,
		Triggers:             meta.Triggers,
		ApplicableTiers:      meta.ApplicableTiers,
		RequiredTools:        meta.RequiredTools,
		RelatedSkills:        meta.RelatedSkills,
		BodyCapLines:         meta.BodyCapLines,
		VerificationStatus:   meta.VerificationStatus,
		FieldRefsVerified:    *meta.FieldRefsVerified,
		IntentLabel:          meta.IntentLabel,
		SkillGroup:           meta.SkillGroup,
		RequiredCitation:     meta.RequiredCitation,
		HandlerKey:           meta.HandlerKey,
		ReactToolSubset:      meta.ReactToolSubset,
		PlannerDirectives:    meta.PlannerDirectives,
		PlannerExamples:      meta.PlannerExamples,
		Provenance:           meta.Provenance,
		ProvenanceTraceRef:   meta.ProvenanceTraceRef,
		SkillVersion:         meta.SkillVersion,
		LastValidatedAgainst: meta.LastValidatedAgainst,
		HumanReviewed:        meta.HumanReviewed,
		Sanitized:            meta.Sanitized,
		EvalPassed:           meta.EvalPassed,
		Resources:            discoverSkillResources(filepath.Dir(path)),
		Path:                 filepath.ToSlash(path),
	}, nil
}

// Fetch returns the skill by name. The boolean is false for an unknown name.
func (l *Loader) Fetch(name string) (*Skill, bool) {
	s, ok := l.skills[name]
	return s, ok
}

// Len is the number of loaded skills.
func (l *Loader) Len() int { return len(l.skills) }

// Names returns the loaded skill names, sorted.
func (l *Loader) Names() []string { return sortedNames(l.skills) }

// Metadata returns the eager planner-visible projection for every loaded skill,
// sorted by name for deterministic prompt construction.
func (l *Loader) Metadata() []SkillMeta {
	out := make([]SkillMeta, 0, len(l.skills))
	for _, name := range sortedNames(l.skills) {
		s := l.skills[name]
		out = append(out, SkillMeta{Name: s.Name, Description: s.Description, Triggers: s.Triggers})
	}
	return out
}

// effectiveBodyCap resolves the body line cap, applying the default when the
// skill omits body_cap_lines.
func (s *Skill) effectiveBodyCap() int {
	if s.BodyCapLines <= 0 {
		return DefaultBodyCapLines
	}
	return s.BodyCapLines
}

// Body lazily reads the skill body, enforces the line cap, and injects the
// caution lines. The (body, error) result is computed once (sync.Once) and
// cached; concurrent callers see the same result. Returns an error (never a
// truncated body) when the authored body exceeds the cap.
func (s *Skill) Body() (string, error) {
	s.bodyOnce.Do(func() { s.body, s.bodyErr = s.loadBody() })
	return s.body, s.bodyErr
}

func (s *Skill) loadBody() (string, error) {
	// Body reads are CWD-independent (B2b P2 forward-risk resolved). Skills loaded
	// by NewLoader carry bodyFS = os.DirFS(root); the generated/runtime registry
	// leaves bodyFS nil and falls back to the package-embedded skillFS. Either way
	// the key is "<name>/SKILL.md" (name==dir enforced at parse), so a deployed
	// binary with CWD != internal/skills resolves the body from the embed instead
	// of a missing relative path (ADR-004 progressive-disclosure loader).
	fsys := s.bodyFS
	if fsys == nil {
		fsys = skillFS
	}
	data, err := fs.ReadFile(fsys, s.Name+"/"+SkillFileName)
	if err != nil {
		return "", fmt.Errorf("skills: read body for %q: %w", s.Name, err)
	}
	_, body, err := splitFrontmatter(data)
	if err != nil {
		return "", fmt.Errorf("skills: split body for %q: %w", s.Name, err)
	}
	body = normalizeNewlines(body)
	if n := countLines(body); n > s.effectiveBodyCap() {
		return "", fmt.Errorf("skills: skill %q body has %d lines, exceeds cap %d (load fails; body is not truncated, ADR-004 section 151)", s.Name, n, s.effectiveBodyCap())
	}
	return s.injectCaution(body), nil
}

// injectCaution is the single choke point for the two ADR-004 caution lines. It
// reads ONLY verification_status and field_refs_verified; never any ADR-008
// evolution field. The half-wired guard test pins that invariant.
func (s *Skill) injectCaution(body string) string {
	if s.VerificationStatus == VerificationUnverified {
		body = CautionUnverified + "\n\n" + body
	}
	if !s.FieldRefsVerified {
		body = strings.TrimRight(body, "\n") + "\n\n" + CautionFieldRefs
	}
	return body
}

func discoverSkillResources(skillDir string) SkillResources {
	return SkillResources{
		Scripts:    discoverResourceFiles(skillDir, "scripts"),
		References: discoverResourceFiles(skillDir, "references"),
		Tests:      discoverResourceFiles(skillDir, "tests"),
	}
}

func discoverResourceFiles(skillDir, subdir string) []string {
	root := filepath.Join(skillDir, subdir)
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil
	}
	var out []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(skillDir, path)
		if relErr != nil {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out
}

func ValidateSkillRegistration(s *Skill) error {
	if s == nil {
		return fmt.Errorf("registration gate: nil skill")
	}
	switch s.Provenance {
	case "", ProvenanceHumanAuthored:
		// Trusted seed/human-authored skills are admitted at codegen time. Runtime
		// import or B9-created skill paths must not rely on this exemption.
		// TODO(B9/#121): require explicit provenance and a stronger sanitization
		// gate before any non-codegen skill import path can register skills.
		return nil
	case ProvenanceDistilledFromTrajectory, ProvenanceAgentCreated:
	default:
		return fmt.Errorf("registration gate: skill %q has unsupported provenance %q", s.Name, s.Provenance)
	}
	if s.ProvenanceTraceRef == "" {
		return fmt.Errorf("registration gate: skill %q requires provenance_trace_ref", s.Name)
	}
	if s.LastValidatedAgainst == "" {
		return fmt.Errorf("registration gate: skill %q requires last_validated_against", s.Name)
	}
	if s.VerificationStatus == VerificationUnverified {
		return fmt.Errorf("registration gate: skill %q must be validated before registration", s.Name)
	}
	if !s.FieldRefsVerified {
		return fmt.Errorf("registration gate: skill %q requires field_refs_verified", s.Name)
	}
	if !s.HumanReviewed {
		return fmt.Errorf("registration gate: skill %q requires human_reviewed", s.Name)
	}
	if !s.Sanitized {
		return fmt.Errorf("registration gate: skill %q requires sanitized", s.Name)
	}
	if !s.EvalPassed {
		return fmt.Errorf("registration gate: skill %q requires eval_passed", s.Name)
	}
	body, err := s.Body()
	if err != nil {
		return fmt.Errorf("registration gate: skill %q body validation: %w", s.Name, err)
	}
	if loc := firstHardcodedIdentifier(body); loc != "" {
		return fmt.Errorf("registration gate: skill %q contains hard-coded identifier %q", s.Name, loc)
	}
	return nil
}

var hardcodedIdentifierRE = regexp.MustCompile(`(?i)\b(uhost-[a-z0-9]+|org-[a-z0-9]+|RetCode\s*[:=]\s*\d+|cn-[a-z0-9]+-[a-z0-9-]+)\b`)

func firstHardcodedIdentifier(s string) string {
	return hardcodedIdentifierRE.FindString(s)
}

// splitFrontmatter splits a `--- ... ---` YAML preamble from the markdown body.
func splitFrontmatter(data []byte) (frontmatter, body string, err error) {
	content := string(data)
	if !strings.HasPrefix(content, "---") {
		return "", "", fmt.Errorf("missing frontmatter `---` opener")
	}
	rest := strings.TrimPrefix(content, "---")
	rest = strings.TrimLeft(rest, "\r\n")
	closer := strings.Index(rest, "\n---")
	if closer < 0 {
		return "", "", fmt.Errorf("missing frontmatter `---` closer")
	}
	frontmatter = rest[:closer]
	body = rest[closer+len("\n---"):]
	body = strings.TrimLeft(body, "\r\n")
	return frontmatter, body, nil
}

func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// countLines counts the lines a body occupies, ignoring trailing blank lines so
// a file's terminal newline does not inflate the count. The PowerShell
// check_skill_caps.ps1 mirrors this rule.
func countLines(s string) int {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

func sortedNames(m map[string]*Skill) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
