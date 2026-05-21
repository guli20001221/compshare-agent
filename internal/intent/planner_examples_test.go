package intent

// C5 Phase A byte-equal contract tests.
//
// Goal: pin that migrating planner one-shot examples from Go literal to
// disk frontmatter does NOT alter the rendered planner prompt. If the
// LLM-facing prompt string drifts by even a byte, ds-v4-flash
// classification can shift under jitter — exactly what these tests
// prevent.
//
// Coverage
// --------
// 1. legacyDiagnosisGroup pins what the migrated data MUST equal,
//    cell-by-cell. Hand-coded as the pre-migration baseline.
// 2. TestPlannerExamples_DiagnosisDiskLoaderEqualsLegacy asserts the
//    disk loader produces a struct deeply equal to the legacy literal.
// 3. TestPlannerExamples_RenderedPromptUnchanged exercises the actual
//    prompt-construction path (renderPlannerPromptExampleGroups +
//    buildSystemPrompt) and asserts the rendered substring for the
//    diagnosis block is byte-equal to the legacy rendering.
// 4. TestPlannerExamples_FullSystemPromptStable hashes the entire
//    buildSystemPrompt() output and asserts it matches a recorded
//    baseline. Any prompt drift — from this migration OR an unrelated
//    edit — fails the test loudly with a diff.
//
// When a future Phase migrates another intent, extend this test by:
//   - moving that group's data into legacyXGroup
//   - adding it to the disk-vs-legacy assertion
//   - re-recording the system prompt hash (review-visible bump)

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacyDiagnosisGroup is the pre-migration plannerPromptExampleGroup
// for IntentDiagnosis. Hand-coded from planner.go at b3d97bf (origin/main).
// MUST match planner_examples/diagnosis.md byte-for-byte after the
// migration. Updating this requires explicit reviewer sign-off.
var legacyDiagnosisGroup = plannerPromptExampleGroup{
	Intent: IntentDiagnosis,
	Source: "Stage 2B diagnosis-vs-knowledge boundary",
	Examples: []plannerPromptExample{
		{
			Question: "uhost-abc123 这台启动失败了帮我查",
			PlanJSON: `{"schema_version":"1.0","intent":"diagnosis","slots":{"target_refs":[{"type":"uhost_id_user_input","value":"uhost-abc123","source":"user_text","source_span":"uhost-abc123"}],"metrics":[],"time_window":null},"required_tools":["DescribeCompShareInstance"],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
			Source:   "Stage 2B: concrete instance target stays diagnosis",
		},
	},
}

// TestPlannerExamples_DiagnosisDiskLoaderEqualsLegacy is the primary
// byte-equal contract — the disk loader must produce a struct
// indistinguishable from the legacy inline literal. If this fails,
// the diagnosis.md frontmatter has drifted from the original Go
// literal at b3d97bf.
func TestPlannerExamples_DiagnosisDiskLoaderEqualsLegacy(t *testing.T) {
	loaded, ok := diskPlannerExampleGroups[IntentDiagnosis]
	require.True(t, ok, "diagnosis.md must load — loader returned no entry for IntentDiagnosis")
	assert.Equal(t, legacyDiagnosisGroup, loaded,
		"disk loader output diverged from the legacy inline literal — "+
			"check internal/intent/planner_examples/diagnosis.md against "+
			"legacyDiagnosisGroup above")
}

// TestPlannerExamples_RenderedPromptUnchanged asserts the production
// rendering path (the one buildSystemPrompt actually uses) produces a
// byte-equal diagnosis substring before and after migration.
func TestPlannerExamples_RenderedPromptUnchanged(t *testing.T) {
	legacyRendered := renderPlannerPromptExampleGroups([]plannerPromptExampleGroup{legacyDiagnosisGroup})
	migrated := diskPlannerExampleGroups[IntentDiagnosis]
	migratedRendered := renderPlannerPromptExampleGroups([]plannerPromptExampleGroup{migrated})
	assert.Equal(t, legacyRendered, migratedRendered,
		"renderPlannerPromptExampleGroups produced different lines for "+
			"legacy vs migrated diagnosis group — prompt drift would shift "+
			"ds-v4-flash classifications")
}

// TestPlannerExamples_FullSystemPromptStable hashes the entire
// buildSystemPrompt() output and asserts it matches a recorded
// baseline. The hash baseline is the output at b3d97bf (origin/main
// pre-PR #86); any change to the planner prompt — from this PR or any
// future PR — must update the baseline and explain why in the commit
// message.
//
// IMPORTANT: when intentionally changing the planner prompt (e.g.
// migrating another intent's examples or adding a directive), this
// test will fail by design. The fix is:
//   1. Run the test, observe the new hash in the failure message.
//   2. Update systemPromptSHA256Baseline below.
//   3. In the commit message, justify the prompt change.
// Don't bypass this gate without a justification — silent prompt
// drift has caused classification regressions in this codebase before.
//
// Baseline captured 2026-05-21 from buildSystemPrompt() at b3d97bf
// origin/main + the PR #86 disk-migration of IntentDiagnosis.
// Migration is byte-equal by construction, so the hash matches the
// pre-migration value.
//
// PR #3 (2026-05-22) — intentional bump: pricing_query capability added
// (intent label + 2 planner_examples + directives + boundary directive
// vs billing_instance). Justification: high-frequency commercial path
// "4090 多少钱" was running through main_react at ~36s/33k tokens on
// baseline; deterministic capability routing brings it to ~10s/6k tokens.
// The boundary directive keeps personal-billing complaints
// ("我账单怎么这么高") routing to billing_instance unchanged.
const systemPromptSHA256Baseline = "4af46ff2a79216ff5a9819d090ed91551e2881620b8ff8480bc157c7a5f94368"

func TestPlannerExamples_FullSystemPromptStable(t *testing.T) {
	prompt := buildSystemPrompt()
	sum := sha256.Sum256([]byte(prompt))
	got := hex.EncodeToString(sum[:])
	if got != systemPromptSHA256Baseline {
		t.Errorf("system prompt drifted.\n"+
			"  baseline: %s\n"+
			"  current:  %s\n"+
			"If you intentionally changed the planner prompt, update "+
			"systemPromptSHA256Baseline and justify in the commit message.",
			systemPromptSHA256Baseline, got)
	}
}

// TestPlannerExamples_FrontmatterRequiresAllFields exercises the
// parser's validation: empty intent, empty source, empty examples,
// missing per-example fields all rejected.
func TestPlannerExamples_FrontmatterRequiresAllFields(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // substring of error
	}{
		{
			"missing_intent",
			"---\nsource: \"x\"\nexamples:\n  - question: q\n    plan_json: '{}'\n    source: s\n---\n",
			"intent must be non-empty",
		},
		{
			"missing_source",
			"---\nintent: diagnosis\nexamples:\n  - question: q\n    plan_json: '{}'\n    source: s\n---\n",
			"source must be non-empty",
		},
		{
			"empty_examples",
			"---\nintent: diagnosis\nsource: \"x\"\nexamples: []\n---\n",
			"examples must be non-empty",
		},
		{
			"example_missing_plan_json",
			"---\nintent: diagnosis\nsource: \"x\"\nexamples:\n  - question: q\n    plan_json: ''\n    source: s\n---\n",
			"plan_json must be non-empty",
		},
		{
			"no_frontmatter",
			"plain markdown with no front matter",
			"missing frontmatter `---` opener",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parsePlannerExampleFrontmatter([]byte(tc.body))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// Sanity check used by future migrations: when intent X moves to
// disk, this helper verifies the disk-backed group is included
// (non-zero) in diskPlannerExampleGroups. Currently only diagnosis.
func TestPlannerExamples_MigratedIntentsArePresent(t *testing.T) {
	migrated := []Intent{IntentDiagnosis}
	for _, intent := range migrated {
		group, ok := diskPlannerExampleGroups[intent]
		require.True(t, ok, "expected disk-backed group for %s", intent)
		assert.NotEmpty(t, group.Examples,
			"%s disk group has zero examples — migration is incomplete", intent)
	}
}

// Compile-time guard: the disk file's YAML keys map to the right
// struct fields. If a future Go-side rename ("PlanJSON" → "Plan") is
// done without updating the YAML, the loader silently drops the
// field. Catch by hand-asserting one known value.
func TestPlannerExamples_DiagnosisExampleJSONLooksValid(t *testing.T) {
	group := diskPlannerExampleGroups[IntentDiagnosis]
	require.Len(t, group.Examples, 1)
	example := group.Examples[0]
	assert.Contains(t, example.PlanJSON, `"intent":"diagnosis"`,
		"plan_json yaml key didn't round-trip to PlanJSON struct field")
	assert.Contains(t, example.PlanJSON, "uhost-abc123",
		"plan_json content didn't survive yaml load")
}

// Self-test for the test harness: legacy + disk renderings produce
// identical line-by-line output even when chained with the prompt's
// per-example wrapping.
func TestPlannerExamples_RenderedLinesByteEqual(t *testing.T) {
	legacy := renderPlannerPromptExampleGroups([]plannerPromptExampleGroup{legacyDiagnosisGroup})
	disk := renderPlannerPromptExampleGroups([]plannerPromptExampleGroup{
		diskPlannerExampleGroups[IntentDiagnosis],
	})
	require.Equal(t, len(legacy), len(disk),
		"line count diverged: legacy=%d disk=%d", len(legacy), len(disk))
	for i := range legacy {
		assert.Equal(t, legacy[i], disk[i],
			"line %d byte-diverged:\n  legacy: %q\n  disk:   %q",
			i, legacy[i], disk[i])
	}
	// Bonus: hash both joined to fail loudly on whitespace drift.
	legacyHash := sha256.Sum256([]byte(strings.Join(legacy, "\n")))
	diskHash := sha256.Sum256([]byte(strings.Join(disk, "\n")))
	assert.Equal(t, legacyHash, diskHash, "joined rendering hash diverged")
}
