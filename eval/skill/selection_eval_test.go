package skill_eval

import (
	"context"
	"flag"
	"os"
	"testing"

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

// selectionPlannerLLM adapts an llm.Client to intent.PlannerLLM, exactly like the
// production cliPlannerLLM (cmd/cli.go): planner requests carry no tools.
type selectionPlannerLLM struct{ client *llm.Client }

func (s selectionPlannerLLM) CompleteIntentPlan(ctx context.Context, req intent.PlannerLLMRequest) (string, error) {
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
//     specific diagnose_* skill is resolved inside ReAct, not at plan time. Plan-time
//     specific-diagnosis selection is R2-v2 (needs an engine-level run).
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

	planner := intent.NewPlanner(
		selectionPlannerLLM{client: llm.NewClient(config.LLMConfig{BaseURL: baseURL, APIKey: apiKey, Model: *skillModelFlag})},
		intent.PlannerOptions{BaseURL: baseURL, Model: *skillModelFlag},
	)

	cases := loadSkillCases(t)

	var selTotal, selHit int
	var diagTotal, diagLaneHit int
	groupTotal := map[string]int{}
	groupWrong := map[string]int{}

	for _, c := range cases {
		result, err := planner.Plan(context.Background(), intent.PlannerInput{UserText: c.Question})
		require.NoErrorf(t, err, "planner error on %s", c.ID)
		selected := selectedSkillName(result.Plan)

		switch c.Lane {
		case laneFast, laneAgent:
			selTotal++
			hit := selected == c.ExpectedSkill
			if hit {
				selHit++
			}
			if c.OverlappingGroup != "" {
				groupTotal[c.OverlappingGroup]++
				if !hit {
					groupWrong[c.OverlappingGroup]++
				}
			}
			t.Logf("[%s] %q expected=%s selected=%s hit=%v", c.ID, c.Question, c.ExpectedSkill, selected, hit)
		case laneDiagnosis:
			diagTotal++
			if result.Plan.Intent == intent.IntentDiagnosis {
				diagLaneHit++
			}
			t.Logf("[%s] %q intent=%s (diagnosis lane-routing only; specific skill is R2-v2)", c.ID, c.Question, result.Plan.Intent)
		}
	}

	rate := func(n, d int) float64 {
		if d == 0 {
			return 0
		}
		return float64(n) / float64(d)
	}
	t.Logf("=== skill-eval selection (model=%s) ===", *skillModelFlag)
	t.Logf("plan-level skill-hit: %d/%d (%.2f)  wrong-skill: %.2f", selHit, selTotal, rate(selHit, selTotal), 1-rate(selHit, selTotal))
	t.Logf("diagnosis lane-routing: %d/%d (%.2f)", diagLaneHit, diagTotal, rate(diagLaneHit, diagTotal))
	for g, total := range groupTotal {
		t.Logf("R4-trigger overlapping group %q wrong-skill rate: %d/%d (%.2f)", g, groupWrong[g], total, rate(groupWrong[g], total))
	}
}

// selectedSkillName returns the concrete plan-time skill for a plan, or "" when
// the skill is resolved later (diagnosis -> resolved_in_react).
func selectedSkillName(plan intent.Plan) string {
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
