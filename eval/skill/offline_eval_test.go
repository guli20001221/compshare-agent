// Package skill_eval is the R2 skill-level evaluation harness. It has two layers
// over ONE shared case file (cases.jsonl):
//
//   - TestOfflineSkillEval (this file) — DETERMINISTIC, runs in CI with no API key
//     and no cloud. For every fast-tier route skill it drives the exported
//     handler seam intent.NewDemoHandler(...).DispatchRoute with the skill's
//     registry intent + the case's raw question, over a recording executor that
//     returns canned read-only data. It asserts the wiring contract: the intent
//     derives the expected skill, the expected read-only tools are called, no
//     forbidden/mutating tool is called, and the deterministic reply contains /
//     excludes the declared keywords. This is the "选得出来但做不了" guard.
//
//   - TestSelectionSkillEval (selection_eval_test.go) — the OPT-IN real-model layer
//     (-skillmodel flag, not CI-gated). It runs the real planner on the raw question
//     to measure skill-hit / wrong-skill rate, including the overlapping-skill subset
//     that is the R4 (description-driven selection) trigger. Real model selection
//     quality cannot be CI-stable, so it is measured on demand like TestEval.
//
// Why the split: the skill-hit / wrong-skill metric is inherently a classifier
// signal. Forcing it into CI with a tuned heuristic would be tautological. So CI
// gates the deterministic wiring (the 6 offline metrics: skill-derivation,
// expected-tool, forbidden/mutating-clean, no-extra-tool, tool-arg, reply-keyword);
// the classifier quality (skill-hit / wrong-skill + the R4 trigger) is the opt-in layer.
package skill_eval

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/routing"
	"github.com/compshare-agent/internal/tools"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SkillCase is one skill-eval case, shared by both layers.
type SkillCase struct {
	ID                    string   `json:"id"`
	Question              string   `json:"question"`
	Lane                  string   `json:"lane"` // fast | diagnosis | agent | boundary
	ExpectedSkill         string   `json:"expected_skill"`
	ExpectedIntent        string   `json:"expected_intent,omitempty"`
	ForbiddenSkills       []string `json:"forbidden_skills,omitempty"`
	ExpectedTools         []string `json:"expected_tools,omitempty"`
	ForbiddenTools        []string `json:"forbidden_tools,omitempty"`
	ReplyShouldContain    []string `json:"reply_should_contain,omitempty"`
	ReplyShouldNotContain []string `json:"reply_should_not_contain,omitempty"`
	// AllowedExtraTools are tools that may be called in addition to ExpectedTools
	// without counting as an extra-tool violation (escape hatch for a handler that
	// legitimately probes more than the case cares to pin). ExpectedTools +
	// AllowedExtraTools is the complete allowed set; anything else is an extra.
	AllowedExtraTools []string `json:"allowed_extra_tools,omitempty"`
	// ExpectedToolArgs pins specific arguments a tool MUST be called with, so a
	// handler that calls the right tool with the wrong GPU / zone / charge type is
	// caught: {action: {paramKey: expectedValue}}. Compared with fmt.Sprint so a
	// JSON number and a Go float compare equal.
	ExpectedToolArgs map[string]map[string]any `json:"expected_tool_args,omitempty"`
	OverlappingGroup string                    `json:"overlapping_group,omitempty"`
	Tags             []string                  `json:"tags,omitempty"`
}

// Lanes.
const (
	laneFast      = "fast"
	laneDiagnosis = "diagnosis"
	laneAgent     = "agent"
	laneBoundary  = "boundary"
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

// findRoute returns the generated deterministic route by name, failing if it is
// not registered.
func findRoute(t *testing.T, name string) *routing.Route {
	t.Helper()
	for _, route := range routing.GeneratedRoutes() {
		if route.Name == name {
			return route
		}
	}
	t.Fatalf("expected_skill %q is not in the generated route registry", name)
	return nil
}

// mutatingActions is the implicitly-forbidden set for every read-only / selection
// skill, DERIVED from the authoritative tool policy (ActionClassMutating /
// ActionClassDestructive) rather than hand-maintained — so a newly added write
// tool is automatically forbidden without editing this test. A read-only handler
// that wires any of these is caught even if the case omits it.
func mutatingActions() []string {
	var out []string
	for action, p := range tools.DefaultToolExecutionPolicies() {
		if p.Class == tools.ActionClassMutating || p.Class == tools.ActionClassDestructive {
			out = append(out, action)
		}
	}
	sort.Strings(out)
	return out
}

// skillMetrics tallies the deterministic CI metrics across cases.
type skillMetrics struct {
	cases               int
	skillDerivationHits int
	expectedToolChecks  int
	expectedToolHits    int
	forbiddenToolChecks int
	forbiddenToolClean  int
	extraToolCases      int // cases with zero unexpected tool calls
	keywordCases        int
	keywordPass         int
	argChecks           int
	argPass             int
}

func (m *skillMetrics) report(t *testing.T) {
	t.Helper()
	pct := func(n, d int) float64 {
		if d == 0 {
			return 1
		}
		return float64(n) / float64(d)
	}
	t.Logf("skill-eval offline: fast_cases=%d skill_derivation=%.2f expected_tool_hit=%.2f forbidden_tool_clean=%.2f no_extra_tool=%.2f tool_arg_pass=%.2f reply_keyword_pass=%.2f",
		m.cases,
		pct(m.skillDerivationHits, m.cases),
		pct(m.expectedToolHits, m.expectedToolChecks),
		pct(m.forbiddenToolClean, m.forbiddenToolChecks),
		pct(m.extraToolCases, m.cases),
		pct(m.argPass, m.argChecks),
		pct(m.keywordPass, m.keywordCases),
	)
}

// TestOfflineSkillEval is the deterministic, CI-stable layer. It exercises every
// fast-tier route skill end-to-end through the exported handler seam with no
// LLM and no network, asserting the per-skill wiring contract.
func TestOfflineSkillEval(t *testing.T) {
	cases := loadSkillCases(t)
	require.NotEmpty(t, mutatingActions(),
		"mutating-tool derivation is empty — the forbidden-tool check would be vacuous")

	var m skillMetrics
	fastSkills := map[string]bool{}
	for _, c := range cases {
		if c.Lane != laneFast {
			continue
		}
		c := c
		t.Run(c.ID, func(t *testing.T) {
			route := findRoute(t, c.ExpectedSkill)
			require.NotEmptyf(t, route.IntentLabel, "route %q has no intent_label", route.Name)
			fastSkills[route.Name] = true

			plan := intent.IntentRoute{Intent: intent.Intent(route.IntentLabel)}

			// (1) routing pin: the intent derives exactly the expected skill.
			derived := intent.DeriveSelectedSkills(plan)
			m.cases++
			if assert.Lenf(t, derived, 1, "intent %q should derive exactly one route label", route.IntentLabel) &&
				assert.Equal(t, c.ExpectedSkill, derived[0].Name, "intent->skill derivation drift") {
				m.skillDerivationHits++
			}

			// (2)+(3) drive the deterministic handler over a recording executor.
			exec := &recordingExecutor{}
			h := intent.NewDemoHandler(exec)
			res := h.DispatchRoute(context.Background(),
				intent.HandlerRequest{Plan: plan, UserText: c.Question})

			called := exec.actions()
			for _, tool := range c.ExpectedTools {
				m.expectedToolChecks++
				if assert.Containsf(t, called, tool, "case %s: expected tool %q not called (called: %v)", c.ID, tool, called) {
					m.expectedToolHits++
				}
			}
			forbidden := append(append([]string{}, c.ForbiddenTools...), mutatingActions()...)
			for _, tool := range forbidden {
				m.forbiddenToolChecks++
				if assert.NotContainsf(t, called, tool, "case %s: forbidden tool %q was called", c.ID, tool) {
					m.forbiddenToolClean++
				}
			}

			// (extra-tool) every called tool must be in ExpectedTools ∪ AllowedExtraTools.
			// Catches a handler that calls the right tool AND also probes unrelated ones.
			allowed := map[string]bool{}
			for _, tool := range append(append([]string{}, c.ExpectedTools...), c.AllowedExtraTools...) {
				allowed[tool] = true
			}
			var extras []string
			for _, tool := range called {
				if !allowed[tool] {
					extras = append(extras, tool)
				}
			}
			if assert.Emptyf(t, extras, "case %s: unexpected tool call(s) %v (allowed: %v)", c.ID, extras, allowed) {
				m.extraToolCases++
			}

			// (tool-args) pin specific args a tool must carry (e.g. pricing GpuType).
			for action, wantArgs := range c.ExpectedToolArgs {
				gotArgs, ok := exec.argsFor(action)
				for key, want := range wantArgs {
					m.argChecks++
					if !ok {
						assert.Failf(t, "missing tool call", "case %s: expected args on %q but it was not called", c.ID, action)
						continue
					}
					if assert.Equalf(t, fmt.Sprint(want), fmt.Sprint(gotArgs[key]),
						"case %s: %s arg %q = %v, want %v", c.ID, action, key, gotArgs[key], want) {
						m.argPass++
					}
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

	// Non-vacuity: every deterministic route in the registry must have at least
	// one case, so a newly added catalog/status route cannot slip in uncovered.
	for _, route := range routing.GeneratedRoutes() {
		assert.Truef(t, fastSkills[route.Name], "route %q has no offline eval case", route.Name)
	}
}
