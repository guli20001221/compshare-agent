package skill_eval

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

// skillModelFlag opts into the real-model selection layer. Empty -> the test
// skips (mirrors eval.TestEval's -model gate). Example:
//
//	go test ./eval/skill -run TestSelectionSkillEval -skillmodel deepseek-v4-flash
var skillModelFlag = flag.String("skillmodel", "", "planner model id for the real-model skill-selection layer (empty = skip)")
var skillRunsFlag = flag.Int("skillruns", 1, "number of repeated real-model selection runs for stability checks")

// selectionPlannerLLM adapts an llm.Client to intent.IntentRouterLLM, exactly like the
// production cliPlannerLLM (cmd/cli.go): planner requests carry no tools.
type selectionPlannerLLM struct{ client *llm.Client }

func (s selectionPlannerLLM) CompleteIntentPlan(ctx context.Context, req intent.IntentRouterLLMRequest) (string, error) {
	resp, err := s.client.Chat(ctx, llm.ChatRequest{
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: req.SystemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: req.UserPrompt},
		},
	})
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// TestSelectionSkillEval is the OPT-IN real-model layer (NOT CI-gated). It runs the
// real planner on each case's raw question and measures how well the model selects
// the right skill — the metric that genuinely tracks "变聪明了 vs 变不稳了" and that
// gates R4 (description-driven selection). Real-model selection cannot be made
// CI-deterministic, so this is run on demand with -skillmodel (like eval.TestEval).
//
// What it measures, per lane:
//   - fast / agent: planner intent -> intent.DeriveSelectedSkills -> concrete skill
//     name, compared to expected_skill. This is skill-hit / wrong-skill.
//   - diagnosis: only the lane (intent == IntentDiagnosis) is checked, because the
//     specific diagnose-* skill is resolved inside ReAct, not at plan time. Plan-time
//     specific-diagnosis selection is R2-v2 (needs an engine-level run).
//   - boundary: planner intent must match expected_intent and the selected skill must
//     not be in forbidden_skills. This catches new skills stealing nearby FAQ/how-to
//     questions without forcing those questions through fast/agent dispatch.
//
// R4 trigger: the per-overlapping-group wrong-skill rate. When a confusable group
// (e.g. the 3-way image_list) crosses a sustained threshold across N>=5 runs, that
// is the evidence that intent enumeration is breaking down and description-driven
// selection (R4) should start.
func TestSelectionSkillEval(t *testing.T) {
	if *skillModelFlag == "" {
		t.Skip("set -skillmodel <model id> (and an LLM API key env) to run the real-model selection layer")
	}
	apiKey := firstNonEmptyEnv("MODELVERSE_API_KEY", "LLM_API_KEY", "LOCAL_PROXY_API_KEY")
	require.NotEmpty(t, apiKey, "need MODELVERSE_API_KEY / LLM_API_KEY for the real-model layer")
	baseURL := firstNonEmptyEnv("LLM_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.modelverse.cn/v1"
	}

	planner := intent.NewIntentRouter(
		selectionPlannerLLM{client: llm.NewClient(config.LLMConfig{BaseURL: baseURL, APIKey: apiKey, Model: *skillModelFlag})},
		intent.IntentRouterOptions{BaseURL: baseURL, Model: *skillModelFlag},
	)

	cases := loadSkillCases(t)

	var selTotal, selHit int
	var diagTotal, diagLaneHit int
	var boundaryTotal, boundaryHit int
	groupTotal := map[string]int{}
	groupWrong := map[string]int{}
	var caseResults []selCaseResult

	runs := *skillRunsFlag
	if runs < 1 {
		runs = 1
	}
	for run := 1; run <= runs; run++ {
		for _, c := range cases {
			result, err := planner.Plan(context.Background(), intent.IntentRouterInput{UserText: c.Question})
			require.NoErrorf(t, err, "planner error on %s run %d", c.ID, run)
			selected := selectedSkillName(result.Plan)
			outcome := evaluateSelectionCase(c, result.Plan, selected)
			outcome.Run = run

			switch c.Lane {
			case laneFast, laneAgent:
				selTotal++
				if outcome.Hit {
					selHit++
				}
				if c.OverlappingGroup != "" {
					groupTotal[c.OverlappingGroup]++
					if !outcome.Hit {
						groupWrong[c.OverlappingGroup]++
					}
				}
				caseResults = append(caseResults, outcome)
				t.Logf("[run %d][%s] %q expected=%s selected=%s hit=%v", run, c.ID, c.Question, c.ExpectedSkill, selected, outcome.Hit)
			case laneDiagnosis:
				diagTotal++
				if outcome.DiagnosisLaneHit {
					diagLaneHit++
				}
				caseResults = append(caseResults, outcome)
				t.Logf("[run %d][%s] %q intent=%s (diagnosis lane-routing only; specific skill is process-eval covered)", run, c.ID, c.Question, result.Plan.Intent)
			case laneBoundary:
				boundaryTotal++
				if outcome.BoundaryHit {
					boundaryHit++
				}
				if c.OverlappingGroup != "" {
					groupTotal[c.OverlappingGroup]++
					if !outcome.Hit {
						groupWrong[c.OverlappingGroup]++
					}
				}
				caseResults = append(caseResults, outcome)
				t.Logf("[run %d][%s] %q expected_intent=%s selected=%s forbidden=%v hit=%v",
					run, c.ID, c.Question, c.ExpectedIntent, selected, c.ForbiddenSkills, outcome.Hit)
			default:
				t.Fatalf("unknown skill-eval lane %q in case %s", c.Lane, c.ID)
			}
		}
	}

	rate := func(n, d int) float64 {
		if d == 0 {
			return 0
		}
		return float64(n) / float64(d)
	}
	report := selectionReport{
		Model:              *skillModelFlag,
		Runs:               runs,
		TimestampUTC:       time.Now().UTC().Format(time.RFC3339),
		SkillHit:           selHit,
		SkillTotal:         selTotal,
		WrongSkillRate:     1 - rate(selHit, selTotal),
		DiagnosisLaneHit:   diagLaneHit,
		DiagnosisLaneTotal: diagTotal,
		BoundaryHit:        boundaryHit,
		BoundaryTotal:      boundaryTotal,
		OverlappingGroups:  map[string]groupRate{},
		Cases:              caseResults,
	}
	for g, total := range groupTotal {
		report.OverlappingGroups[g] = groupRate{Wrong: groupWrong[g], Total: total, WrongRate: rate(groupWrong[g], total)}
	}

	t.Logf("=== skill-eval selection (model=%s) ===", *skillModelFlag)
	t.Logf("runs: %d", runs)
	t.Logf("plan-level skill-hit: %d/%d (%.2f)  wrong-skill: %.2f", selHit, selTotal, rate(selHit, selTotal), report.WrongSkillRate)
	t.Logf("diagnosis lane-routing: %d/%d (%.2f)", diagLaneHit, diagTotal, rate(diagLaneHit, diagTotal))
	t.Logf("boundary cases: %d/%d (%.2f)", boundaryHit, boundaryTotal, rate(boundaryHit, boundaryTotal))
	for g, gr := range report.OverlappingGroups {
		t.Logf("R4-trigger overlapping group %q wrong-skill rate: %d/%d (%.2f)", g, gr.Wrong, gr.Total, gr.WrongRate)
	}

	// Trackable JSON report: written to $SKILL_EVAL_REPORT for run-over-run
	// comparison (the R4 trigger watches the overlapping-group wrong-skill rate),
	// or logged inline when the env var is unset.
	blob, err := json.MarshalIndent(report, "", "  ")
	require.NoError(t, err)
	if path := os.Getenv("SKILL_EVAL_REPORT"); path != "" {
		require.NoError(t, os.WriteFile(path, blob, 0o644))
		t.Logf("selection report written to %s", path)
	} else {
		t.Logf("selection report (set SKILL_EVAL_REPORT=path to persist):\n%s", blob)
	}
}

type selCaseResult struct {
	Run              int      `json:"run"`
	ID               string   `json:"id"`
	Lane             string   `json:"lane"`
	Question         string   `json:"question"`
	ExpectedSkill    string   `json:"expected_skill"`
	ExpectedIntent   string   `json:"expected_intent,omitempty"`
	ForbiddenSkills  []string `json:"forbidden_skills,omitempty"`
	SelectedSkill    string   `json:"selected_skill,omitempty"`
	Intent           string   `json:"intent"`
	OverlappingGroup string   `json:"overlapping_group,omitempty"`
	Hit              bool     `json:"hit,omitempty"`
	DiagnosisLaneHit bool     `json:"diagnosis_lane_hit,omitempty"`
	BoundaryHit      bool     `json:"boundary_hit,omitempty"`
}

type groupRate struct {
	Wrong     int     `json:"wrong"`
	Total     int     `json:"total"`
	WrongRate float64 `json:"wrong_rate"`
}

type selectionReport struct {
	Model              string               `json:"model"`
	Runs               int                  `json:"runs"`
	TimestampUTC       string               `json:"timestamp_utc"`
	SkillHit           int                  `json:"skill_hit"`
	SkillTotal         int                  `json:"skill_total"`
	WrongSkillRate     float64              `json:"wrong_skill_rate"`
	DiagnosisLaneHit   int                  `json:"diagnosis_lane_hit"`
	DiagnosisLaneTotal int                  `json:"diagnosis_lane_total"`
	BoundaryHit        int                  `json:"boundary_hit"`
	BoundaryTotal      int                  `json:"boundary_total"`
	OverlappingGroups  map[string]groupRate `json:"overlapping_groups"`
	Cases              []selCaseResult      `json:"cases"`
}

func evaluateSelectionCase(c SkillCase, plan intent.IntentRoute, selected string) selCaseResult {
	result := selCaseResult{
		ID:               c.ID,
		Lane:             c.Lane,
		Question:         c.Question,
		ExpectedSkill:    c.ExpectedSkill,
		ExpectedIntent:   c.ExpectedIntent,
		ForbiddenSkills:  c.ForbiddenSkills,
		SelectedSkill:    selected,
		Intent:           string(plan.Intent),
		OverlappingGroup: c.OverlappingGroup,
	}
	switch c.Lane {
	case laneFast, laneAgent:
		result.Hit = selected == c.ExpectedSkill
	case laneDiagnosis:
		result.DiagnosisLaneHit = plan.Intent == intent.IntentDiagnosis
		result.Hit = result.DiagnosisLaneHit
	case laneBoundary:
		intentOK := c.ExpectedIntent == "" || string(plan.Intent) == c.ExpectedIntent
		forbiddenClean := true
		for _, forbidden := range c.ForbiddenSkills {
			if selected == forbidden {
				forbiddenClean = false
				break
			}
		}
		result.BoundaryHit = intentOK && forbiddenClean
		result.Hit = result.BoundaryHit
	}
	return result
}

// selectedSkillName returns the concrete plan-time skill for a plan, or "" when
// the skill is resolved later (diagnosis -> resolved_in_react).
func selectedSkillName(plan intent.IntentRoute) string {
	for _, s := range intent.DeriveSelectedSkills(plan) {
		if s.Name != "" {
			return s.Name
		}
	}
	return ""
}

func firstNonEmptyEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
