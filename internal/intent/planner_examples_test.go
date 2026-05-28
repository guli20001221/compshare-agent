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
//  1. Run the test, observe the new hash in the failure message.
//  2. Update systemPromptSHA256Baseline below.
//  3. In the commit message, justify the prompt change.
//
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
//
// PR #3 amend (2026-05-22 post-review) — second bump: reviewer caught
// (a) the allowed-intent enum on line 414 still missing pricing_query
// (doc correctness — observed not to block emission but kept for
// consistency with the enum-as-contract pattern); (b) the stale
// directive on line 422 that conflicted with the new capability example
// ("4090 多少钱 → unknown"). Replaced with a pricing_query directive +
// explicit personal-billing-complaints boundary preserving billing_instance.
//
// R3-A1 (2026-05-24) — third bump: 6 modelverse model-API anchors added to
// the knowledge_qa group (Suno/Vidu/flux/gpt-image/minimax-speech/error-code).
// Justification: R2 CLI smoke (PR #165 verification) showed 5/12 modelverse
// model-API questions being classified as IntentUnknown — engine.go:1450
// `tryStage2BRetrieval` short-circuits unless Plan.Intent == IntentKnowledgeQA,
// so RAG never ran against the 46 modelverse chunks shipped in PR #165 and
// users got "我不知道"-class fallbacks instead. Group `Source` updated to
// flag the addition; SHA bumped by-construction. Boundary: anchors phrase
// "X 怎么调 / 用 SDK 怎么传参 / 返回 N 是什么错误" so they don't conflict
// with billing/stock/pricing/diagnosis intents (no $-amount, no instance ID,
// no "我的" personal-status markers).
// L2 prompt tiering (2026-05-27): knowledge_qa compact rendering — 20 examples
// share one plan JSON template + question list instead of repeating the full
// JSON 20 times. All 20 boundary anchor questions preserved; saves ~1,100
// planner prompt tokens (~27% reduction). Bump justified: no examples removed,
// only rendering format changed for the knowledge_qa group.
//
// Batch 1 (2026-05-28) — operation_lifecycle jitter fix: added one new
// example group with 5 anchors (帮我关机 uhost-xxx / uhost-test 停了 / 启动
// train-gpu / 把 uhost-xxx 重启一下 / 给 uhost-xxx 加 200G 数据盘) plus a
// directive sentence so the classifier stops drifting UHostId+action-verb
// chats to unknown. Pre-fix: 67% drift at N=6 trace; post-fix anchor groups
// match the billing_instance pattern that fixed the Q04 jitter. SHA bumped
// by-construction.
//
// PR1 hotfix Bug 1 (2026-05-28): the Batch 1 anchors all carried a target_ref
// (UHostId or name), so the bare "帮我关机" without an instance still drifted
// to unknown. This bump adds a 6th operation_lifecycle anchor with
// target_refs:[] and revises the directive — "action verb alone is sufficient,
// regardless of target presence" — pointing the engine at the
// list-and-prompt fallback when the user omits the instance reference.
// See memory:target-ref-required-for-operation-lifecycle.
//
// PR1 hotfix Bug 4 was reverted on the planner side (2026-05-28 PM 联调):
// adding "also emit slots.action with the matching verb" + the action field
// in 6 examples caused ds-v4-flash to respond with 800-1800 token prose
// instead of JSON (output_tok 805/1475/1823 vs normal 165-241 for working
// intents), tipping every operation_lifecycle turn into schema_valid=false
// → unknown → ReAct fallback. The Go types (Slots.Action / LifecycleAction /
// LifecycleAction* constants) and engine-side filterDescribeResultByAction
// remain in place as dead code so a more careful future re-introduction can
// wire them back through a different schema or a stronger JSON-only nudge.
// SHA accordingly returned to the Bug 1 anchor baseline.
const systemPromptSHA256Baseline = "b9ac2d5f6906fea3f38a3f88dc15e51ab69d7978518200dbdd3d12650856202e"

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
// (non-zero) in diskPlannerExampleGroups. Currently diagnosis +
// knowledge_qa (Phase A + Phase B).
func TestPlannerExamples_MigratedIntentsArePresent(t *testing.T) {
	migrated := []Intent{IntentDiagnosis, IntentKnowledgeQA}
	for _, intent := range migrated {
		group, ok := diskPlannerExampleGroups[intent]
		require.True(t, ok, "expected disk-backed group for %s", intent)
		assert.NotEmpty(t, group.Examples,
			"%s disk group has zero examples — migration is incomplete", intent)
	}
}

// legacyKnowledgeQAGroup is the pre-migration plannerPromptExampleGroup
// for IntentKnowledgeQA. Hand-coded from planner.go at 19a692d
// (claude/tool-retry-timeout head, which inherits from PR #151+#152).
// MUST match planner_examples/knowledge_qa.md byte-for-byte after the
// migration. Updating this requires explicit reviewer sign-off.
var legacyKnowledgeQAGroup = plannerPromptExampleGroup{
	Intent:  IntentKnowledgeQA,
	Source:  "Stage 2B + PR #34a/#52/#60 knowledge_qa routing regressions + R3-A1 modelverse model-API coverage",
	compact: true,
	Examples: []plannerPromptExample{
		{
			Question: "为啥显卡内存满了 GPU 占用才 10%",
			PlanJSON: `{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
			Source:   "PR #60: concept question with monitor-trigger words",
		},
		{
			Question: "how do I issue an invoice",
			PlanJSON: `{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
			Source:   "PR #52: finance process question, not personal status",
		},
		{
			Question: "what image types does the platform provide",
			PlanJSON: `{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
			Source:   "Stage 2B: platform concept question",
		},
		{
			Question: "远程桌面没声音该怎么处理",
			PlanJSON: `{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
			Source:   "Stage 2B: platform how-to/config boundary",
		},
		{
			Question: "错误码 226601 是什么意思",
			PlanJSON: `{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
			Source:   "Stage 2B: error-code knowledge question",
		},
		{
			Question: "Linux 怎么装 NVIDIA 驱动",
			PlanJSON: `{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
			Source:   "Stage 2B: platform how-to/config boundary",
		},
		{
			Question: "Coding Plan 的 BaseURL 应该填什么",
			PlanJSON: `{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
			Source:   "Stage 2B: model API configuration",
		},
		{
			Question: "怎么在 VSCode 里连 GPU 实例",
			PlanJSON: `{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
			Source:   "Stage 2B: connection how-to",
		},
		{
			Question: "在 CLINE 里加 mcp-server-sqlite 那段 json 该怎么写",
			PlanJSON: `{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
			Source:   "PR #60: third-party tool configuration jargon",
		},
		{
			Question: "怎么查我这个月的账单",
			PlanJSON: `{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
			Source:   "PR #52: billing navigation question",
		},
		{
			Question: "哪里可以看发票发起记录",
			PlanJSON: `{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
			Source:   "PR #52: invoice navigation question",
		},
		{
			Question: "包月和按量哪个划算",
			PlanJSON: `{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
			Source:   "PR #34a: platform comparison question",
		},
		{
			Question: "实例磁盘可以扩容吗",
			PlanJSON: `{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
			Source:   "PR #34a: platform feasibility question",
		},
		{
			Question: "退款流程是怎样的",
			PlanJSON: `{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
			Source:   "PR #34a: platform procedure question",
		},
		{
			Question: "Suno 怎么用 API 生成歌曲",
			PlanJSON: `{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
			Source:   "R3-A1: modelverse music-gen API (Suno)",
		},
		{
			Question: "Vidu 接口怎么传图生成视频",
			PlanJSON: `{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
			Source:   "R3-A1: modelverse video-gen API (Vidu)",
		},
		{
			Question: "flux 模型调用 API 怎么传 prompt 和图",
			PlanJSON: `{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
			Source:   "R3-A1: modelverse image-gen API (flux)",
		},
		{
			Question: "用 OpenAI SDK 调 gpt-image-1 怎么传参",
			PlanJSON: `{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
			Source:   "R3-A1: modelverse OpenAI-compat API (gpt-image)",
		},
		{
			Question: "minimax-speech 怎么生成中文语音",
			PlanJSON: `{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
			Source:   "R3-A1: modelverse TTS API (minimax-speech)",
		},
		{
			Question: "modelverse 返回 1002 是什么错误",
			PlanJSON: `{"schema_version":"1.0","intent":"knowledge_qa","slots":{"target_refs":[],"metrics":[],"time_window":null},"required_tools":[],"retrieval":{"enabled":false},"hard_block_hint":false,"confidence":0.85}`,
			Source:   "R3-A1: modelverse error-code reference",
		},
	},
}

// TestPlannerExamples_KnowledgeQADiskLoaderEqualsLegacy is the Phase B
// byte-equal contract — disk loader output for IntentKnowledgeQA must
// be indistinguishable from the legacy inline literal. If this fails,
// knowledge_qa.md frontmatter has drifted from the original Go literal.
func TestPlannerExamples_KnowledgeQADiskLoaderEqualsLegacy(t *testing.T) {
	loaded, ok := diskPlannerExampleGroups[IntentKnowledgeQA]
	require.True(t, ok, "knowledge_qa.md must load — loader returned no entry for IntentKnowledgeQA")
	assert.Equal(t, legacyKnowledgeQAGroup, loaded,
		"disk loader output diverged from the legacy inline literal — "+
			"check internal/intent/planner_examples/knowledge_qa.md against "+
			"legacyKnowledgeQAGroup above")
}

// TestPlannerExamples_KnowledgeQARenderedPromptUnchanged asserts the
// production rendering path produces byte-equal output for the legacy
// inline literal vs the disk-loaded group.
func TestPlannerExamples_KnowledgeQARenderedPromptUnchanged(t *testing.T) {
	legacyRendered := renderPlannerPromptExampleGroups([]plannerPromptExampleGroup{legacyKnowledgeQAGroup})
	migrated := diskPlannerExampleGroups[IntentKnowledgeQA]
	migratedRendered := renderPlannerPromptExampleGroups([]plannerPromptExampleGroup{migrated})
	assert.Equal(t, legacyRendered, migratedRendered,
		"renderPlannerPromptExampleGroups produced different lines for "+
			"legacy vs migrated knowledge_qa group — prompt drift would shift "+
			"ds-v4-flash classifications")
}

// Compile-time guard: knowledge_qa.md YAML keys map to the right struct
// fields. Counterpart to TestPlannerExamples_DiagnosisExampleJSONLooksValid.
func TestPlannerExamples_KnowledgeQAExamplesJSONLookValid(t *testing.T) {
	group := diskPlannerExampleGroups[IntentKnowledgeQA]
	require.Len(t, group.Examples, 20, "knowledge_qa.md must have 20 examples (14 legacy + 6 R3-A1 modelverse model-API anchors)")
	for i, ex := range group.Examples {
		assert.Contains(t, ex.PlanJSON, `"intent":"knowledge_qa"`,
			"example[%d] plan_json yaml key didn't round-trip", i)
		assert.NotEmpty(t, ex.Question, "example[%d].question empty", i)
		assert.NotEmpty(t, ex.Source, "example[%d].source empty", i)
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
