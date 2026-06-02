// Package skill_eval is the R2 skill-level evaluation harness. It has two layers
// over ONE shared case file (cases.jsonl):
//
//   - TestOfflineSkillEval (this file) — DETERMINISTIC, runs in CI with no API key
//     and no cloud. For every fast-tier capability skill it drives the exported
//     handler seam intent.NewDemoHandler(...).DispatchCapability with the skill's
//     registry intent + the case's raw question, over a recording executor that
//     returns canned read-only data. It asserts the wiring contract: the intent
//     derives the expected skill, the expected read-only tools are called, no
//     forbidden/mutating tool is called, and the deterministic reply contains /
//     excludes the declared keywords. This is the "选得出来但做不了" guard.
//
//   - TestSelectionSkillEval (selection_eval_test.go) — the OPT-IN real-model layer
//     (-model flag, not CI-gated). It runs the real planner on the raw question to
//     measure skill-hit / wrong-skill rate, including the overlapping-skill subset
//     that is the R4 (description-driven selection) trigger. Real model selection
//     quality cannot be CI-stable, so it is measured on demand like TestEval.
//
// Why the split: the skill-hit / wrong-skill metric is inherently a classifier
// signal. Forcing it into CI with a tuned heuristic would be tautological. So CI
// gates the deterministic wiring (4 metrics); the real classifier quality (the
// 5th metric, the R4 trigger) is the opt-in layer.
package skill_eval

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/skills"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SkillCase is one skill-eval case, shared by both layers.
type SkillCase struct {
	ID                    string   `json:"id"`
	Question              string   `json:"question"`
	Lane                  string   `json:"lane"` // fast | diagnosis | agent
	ExpectedSkill         string   `json:"expected_skill"`
	ExpectedTools         []string `json:"expected_tools,omitempty"`
	ForbiddenTools        []string `json:"forbidden_tools,omitempty"`
	ReplyShouldContain    []string `json:"reply_should_contain,omitempty"`
	ReplyShouldNotContain []string `json:"reply_should_not_contain,omitempty"`
	OverlappingGroup      string   `json:"overlapping_group,omitempty"`
	Tags                  []string `json:"tags,omitempty"`
}

// Lanes.
const (
	laneFast      = "fast"
	laneDiagnosis = "diagnosis"
	laneAgent     = "agent"
)

func loadSkillCases(t *testing.T) []SkillCase {
	t.Helper()
	f, err := os.Open("cases.jsonl")
	require.NoError(t, err, "open cases.jsonl")
	defer f.Close()

	var cases []SkillCase
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	seen := map[string]bool{}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		var c SkillCase
		require.NoErrorf(t, json.Unmarshal([]byte(line), &c), "parse case line: %s", line)
		require.NotEmptyf(t, c.ID, "case missing id: %s", line)
		require.Falsef(t, seen[c.ID], "duplicate case id %q", c.ID)
		seen[c.ID] = true
		cases = append(cases, c)
	}
	require.NoError(t, sc.Err())
	require.GreaterOrEqual(t, len(cases), 12, "expected at least one case per skill")
	return cases
}

// findSkill returns the generated skill by name, failing if it is not registered.
func findSkill(t *testing.T, name string) *skills.Skill {
	t.Helper()
	for _, s := range skills.GeneratedSkills() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("expected_skill %q is not in the generated registry", name)
	return nil
}

// mutatingTools must NEVER be called by a read-only / selection skill. The
// offline layer treats them as implicitly forbidden for every case so a handler
// that wires a write path is caught even if the case omits it. Sourced from the
// mutating set gated by COMPSHARE_ENABLE_MUTATING_TOOLS (internal/tools/registry.go).
var mutatingTools = []string{
	"CreateCompShareInstance",
	"CreateInstanceWorkflow",
	"StartInstanceWorkflow",
	"StopInstanceWorkflow",
	"RebootInstanceWorkflow",
	"RenameInstanceWorkflow",
	"ResetPasswordWorkflow",
	"SetStopSchedulerWorkflow",
	"CancelStopSchedulerWorkflow",
	"ResizeInstanceWorkflow",
	"ReinstallInstanceWorkflow",
	"CreateDiskWorkflow",
}

// skillMetrics tallies the deterministic CI metrics across cases.
type skillMetrics struct {
	cases               int
	skillDerivationHits int
	expectedToolChecks  int
	expectedToolHits    int
	forbiddenToolChecks int
	forbiddenToolClean  int
	keywordCases        int
	keywordPass         int
}

func (m *skillMetrics) report(t *testing.T) {
	t.Helper()
	pct := func(n, d int) float64 {
		if d == 0 {
			return 1
		}
		return float64(n) / float64(d)
	}
	t.Logf("skill-eval offline: fast_cases=%d skill_derivation=%.2f expected_tool_hit=%.2f forbidden_tool_clean=%.2f reply_keyword_pass=%.2f",
		m.cases,
		pct(m.skillDerivationHits, m.cases),
		pct(m.expectedToolHits, m.expectedToolChecks),
		pct(m.forbiddenToolClean, m.forbiddenToolChecks),
		pct(m.keywordPass, m.keywordCases),
	)
}

// TestOfflineSkillEval is the deterministic, CI-stable layer. It exercises every
// fast-tier capability skill end-to-end through the exported handler seam with no
// LLM and no network, asserting the per-skill wiring contract.
func TestOfflineSkillEval(t *testing.T) {
	cases := loadSkillCases(t)

	var m skillMetrics
	fastSkills := map[string]bool{}
	for _, c := range cases {
		if c.Lane != laneFast {
			continue
		}
		c := c
		t.Run(c.ID, func(t *testing.T) {
			skill := findSkill(t, c.ExpectedSkill)
			require.NotEmptyf(t, skill.IntentLabel, "fast-tier skill %q has no intent_label", skill.Name)
			fastSkills[skill.Name] = true

			plan := intent.Plan{Intent: intent.Intent(skill.IntentLabel)}

			// (1) routing pin: the intent derives exactly the expected skill.
			derived := intent.DeriveSelectedSkills(plan)
			m.cases++
			if assert.Lenf(t, derived, 1, "intent %q should derive exactly one skill", skill.IntentLabel) &&
				assert.Equal(t, c.ExpectedSkill, derived[0].Name, "intent->skill derivation drift") {
				m.skillDerivationHits++
			}

			// (2)+(3) drive the deterministic handler over a recording executor.
			exec := &recordingExecutor{}
			h := intent.NewDemoHandler(exec)
			res := h.DispatchCapability(context.Background(),
				intent.HandlerRequest{Plan: plan, UserText: c.Question})

			for _, tool := range c.ExpectedTools {
				m.expectedToolChecks++
				if assert.Containsf(t, exec.calls, tool, "case %s: expected tool %q not called (called: %v)", c.ID, tool, exec.calls) {
					m.expectedToolHits++
				}
			}
			forbidden := append(append([]string{}, c.ForbiddenTools...), mutatingTools...)
			for _, tool := range forbidden {
				m.forbiddenToolChecks++
				if assert.NotContainsf(t, exec.calls, tool, "case %s: forbidden tool %q was called", c.ID, tool) {
					m.forbiddenToolClean++
				}
			}

			// (4) deterministic reply keyword checks.
			if len(c.ReplyShouldContain)+len(c.ReplyShouldNotContain) > 0 {
				m.keywordCases++
				ok := true
				for _, kw := range c.ReplyShouldContain {
					if !assert.Containsf(t, res.Reply, kw, "case %s: reply missing %q\nreply: %s", c.ID, kw, res.Reply) {
						ok = false
					}
				}
				for _, kw := range c.ReplyShouldNotContain {
					if !assert.NotContainsf(t, res.Reply, kw, "case %s: reply must not contain %q", c.ID, kw) {
						ok = false
					}
				}
				if ok {
					m.keywordPass++
				}
			}
		})
	}

	m.report(t)

	// Non-vacuity: every fast-tier skill in the registry must have at least one
	// case, so a newly added catalog skill cannot slip in uncovered.
	for _, s := range skills.GeneratedSkills() {
		if slices.Contains(s.ApplicableTiers, skills.TierFast) {
			assert.Truef(t, fastSkills[s.Name], "fast-tier skill %q has no offline eval case", s.Name)
		}
	}
}
