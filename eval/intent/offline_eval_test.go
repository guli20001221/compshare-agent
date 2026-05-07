package intent_eval

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/entity"
	intp "github.com/compshare-agent/internal/intent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOfflineFixturesEval(t *testing.T) {
	fixtures := loadFixtures(t, "fixtures.jsonl")
	require.GreaterOrEqual(t, len(fixtures), 50)

	planner := intp.NewPlanner(&heuristicFixtureLLM{}, intp.PlannerOptions{
		BaseURL: "https://api.modelverse.cn/v1",
		Model:   "Qwen/Qwen3-Max",
	})

	var legal, targetTotal, targetCorrect int
	for _, fx := range fixtures {
		reg := registryFromFixture(t, fx.RegistrySnapshot)
		result, err := planner.Plan(context.Background(), intp.PlannerInput{
			UserText: fx.UserMsg,
			Registry: reg,
		})
		if assert.NoError(t, err, fx.ID) && !result.Fallback {
			legal++
		}
		if isTargetIntent(fx.ExpectedPlan.Intent) {
			targetTotal++
			if result.Plan.Intent == fx.ExpectedPlan.Intent {
				targetCorrect++
			}
		}
		assert.Equal(t, normalizeTools(fx.ExpectedPlan.RequiredTools), normalizeTools(result.Plan.RequiredTools), fx.ID)
	}

	legalRate := float64(legal) / float64(len(fixtures))
	targetAccuracy := float64(targetCorrect) / float64(targetTotal)
	t.Logf("intent offline eval: fixtures=%d legal_rate=%.2f target_accuracy=%.2f target_correct=%d/%d",
		len(fixtures), legalRate, targetAccuracy, targetCorrect, targetTotal)
	assert.GreaterOrEqual(t, legalRate, 0.95)
	assert.GreaterOrEqual(t, targetAccuracy, 0.90)
}

type fixture struct {
	ID               string                    `json:"id"`
	UserMsg          string                    `json:"user_msg"`
	RegistrySnapshot []entity.InstanceSnapshot `json:"registry_snapshot"`
	ExpectedPlan     expectedPlan              `json:"expected_plan"`
}

type expectedPlan struct {
	Intent        intp.Intent `json:"intent"`
	RequiredTools []string    `json:"required_tools"`
}

func loadFixtures(t *testing.T, path string) []fixture {
	t.Helper()
	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()

	var fixtures []fixture
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var fx fixture
		require.NoError(t, json.Unmarshal([]byte(line), &fx), line)
		fixtures = append(fixtures, fx)
	}
	require.NoError(t, scanner.Err())
	return fixtures
}

func registryFromFixture(t *testing.T, snapshots []entity.InstanceSnapshot) *entity.EntityRegistry {
	t.Helper()
	set := make([]any, 0, len(snapshots))
	for _, inst := range snapshots {
		set = append(set, map[string]any{
			"UHostId": inst.UHostId,
			"Name":    inst.Name,
			"State":   inst.State,
			"GpuType": inst.GpuType,
			"GPU":     float64(inst.GPU),
		})
	}
	reg := entity.NewRegistry()
	require.NoError(t, reg.SyncFromDescribe(map[string]any{
		"TotalCount": float64(len(set)),
		"UHostSet":   set,
	}, "fixture"))
	return reg
}

type heuristicFixtureLLM struct{}

func (h *heuristicFixtureLLM) CompleteIntentPlan(_ context.Context, req intp.PlannerLLMRequest) (string, error) {
	msg := extractUserMessage(req.UserPrompt)
	plan := classifyFixtureMessage(msg)
	data, err := json.Marshal(plan)
	return string(data), err
}

func classifyFixtureMessage(msg string) intp.Plan {
	normalized := strings.ToLower(msg)
	switch {
	case isAccountBillingUnsupportedText(normalized):
		return intp.Plan{
			SchemaVersion: intp.SchemaVersion,
			Intent:        intp.IntentBillingAccountUnsupported,
			Retrieval:     intp.Retrieval{Enabled: false},
			HardBlockHint: true,
			Confidence:    0.9,
		}
	case isBillingInstanceText(normalized):
		return intp.Plan{
			SchemaVersion: intp.SchemaVersion,
			Intent:        intp.IntentBillingInstance,
			Slots: intp.Slots{TargetRefs: []intp.TargetRef{{
				Type:  intp.TargetRefFilter,
				Value: "all",
			}}},
			RequiredTools: []string{"DescribeCompShareInstance", "DiagnoseBilling"},
			Retrieval:     intp.Retrieval{Enabled: false},
			Confidence:    0.86,
		}
	case isMonitorText(normalized):
		return intp.Plan{
			SchemaVersion: intp.SchemaVersion,
			Intent:        intp.IntentMonitorQuery,
			Slots: intp.Slots{
				TargetRefs: []intp.TargetRef{{
					Type:  intp.TargetRefFilter,
					Value: "all_running",
				}},
				Metrics: []intp.Metric{intp.MetricCPU, intp.MetricMemory, intp.MetricGPU, intp.MetricVRAM},
				TimeWindow: &intp.TimeWindow{
					Type:  intp.TimeWindowPreset,
					Value: "last_60s",
				},
			},
			RequiredTools: []string{"DescribeCompShareInstance", "GetCompShareInstanceMonitor"},
			Retrieval:     intp.Retrieval{Enabled: false},
			Confidence:    0.88,
		}
	default:
		return intp.Plan{
			SchemaVersion: intp.SchemaVersion,
			Intent:        intp.IntentUnknown,
			Retrieval:     intp.Retrieval{Enabled: false},
			Confidence:    0.2,
		}
	}
}

func extractUserMessage(prompt string) string {
	const marker = "用户问题："
	idx := strings.Index(prompt, marker)
	if idx < 0 {
		return prompt
	}
	msg := prompt[idx+len(marker):]
	if next := strings.Index(msg, "\n"); next >= 0 {
		msg = msg[:next]
	}
	return msg
}

func isMonitorText(s string) bool {
	return strings.Contains(s, "监控") ||
		strings.Contains(s, "cpu") ||
		strings.Contains(s, "gpu") ||
		strings.Contains(s, "显存") ||
		strings.Contains(s, "利用率") ||
		strings.Contains(s, "使用率")
}

func isBillingInstanceText(s string) bool {
	return strings.Contains(s, "关机后还在扣费") ||
		strings.Contains(s, "哪台实例消费") ||
		strings.Contains(s, "机器费用") ||
		strings.Contains(s, "实例费用") ||
		strings.Contains(s, "计费") ||
		strings.Contains(s, "扣费") ||
		strings.Contains(s, "扣费最多") ||
		strings.Contains(s, "费用占比")
}

func isAccountBillingUnsupportedText(s string) bool {
	return strings.Contains(s, "余额") ||
		strings.Contains(s, "balance") ||
		strings.Contains(s, "账单明细") ||
		strings.Contains(s, "总共消费") ||
		strings.Contains(s, "总共扣") ||
		strings.Contains(s, "总账单") ||
		strings.Contains(s, "消费明细") ||
		strings.Contains(s, "消费流水") ||
		strings.Contains(s, "本月总账单")
}

func isTargetIntent(intent intp.Intent) bool {
	return intent == intp.IntentMonitorQuery ||
		intent == intp.IntentBillingInstance ||
		intent == intp.IntentBillingAccountUnsupported
}

func normalizeTools(tools []string) []string {
	if tools == nil {
		return []string{}
	}
	return tools
}
