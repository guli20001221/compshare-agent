package intent_eval

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/entity"
	intp "github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/observability"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShadowMonitorFixturesEval(t *testing.T) {
	fixtures := loadShadowMonitorFixtures(t, "shadow_monitor_fixtures.jsonl")
	require.GreaterOrEqual(t, len(fixtures), 8)

	planner := intp.NewPlanner(&shadowMonitorFixtureLLM{}, intp.PlannerOptions{
		BaseURL: "https://api.modelverse.cn/v1",
		Model:   "deepseek-v4-flash",
	})

	for _, fx := range fixtures {
		t.Run(fx.ID, func(t *testing.T) {
			require.GreaterOrEqual(t, len(fx.Turns), 2)
			reg := registryFromFixture(t, fx.RegistrySnapshot)
			current := fx.Turns[len(fx.Turns)-1].UserMsg
			result, err := planner.Plan(context.Background(), intp.PlannerInput{
				UserText:  current,
				PriorText: shadowPriorText(fx.Turns[:len(fx.Turns)-1]),
				Resolver:  reg.Snapshot(),
			})
			require.NoError(t, err)
			require.False(t, result.Fallback)
			assert.Equal(t, fx.ExpectedIntent, result.Plan.Intent)

			record := shadowTraceRecordFromFixture(fx, intp.ProjectPlannerTrace(result, intp.PlannerTraceOptions{
				Enabled: true,
				Model:   "deepseek-v4-flash",
			}))
			report := evaluateShadowMonitorRecord(record)

			if fx.RequireMonitorIntent {
				assert.True(t, isShadowMonitorIntent(record.Planner.Intent), "turn 2 should be monitor intent")
				for _, metric := range fx.ExpectedMetrics {
					assert.Contains(t, record.Planner.Slots.Metrics, metric)
				}
			}
			if fx.ExpectHardBlock {
				assert.True(t, record.EngineHardBlock.Hit)
				assert.Equal(t, "account_billing_unsupported", record.EngineHardBlock.Category)
			}
			assert.Equal(t, fx.ExpectMonitorFreshnessMiss, report.MonitorFreshnessMiss)
		})
	}
}

type shadowMonitorFixture struct {
	ID                         string                    `json:"id"`
	Turns                      []shadowFixtureTurn       `json:"turns"`
	RegistrySnapshot           []entity.InstanceSnapshot `json:"registry_snapshot"`
	ExpectedIntent             intp.Intent               `json:"expected_intent"`
	ExpectedMetrics            []string                  `json:"expected_metrics,omitempty"`
	ProductionToolActionsTurn2 []string                  `json:"production_tool_actions_turn2,omitempty"`
	ExpectMonitorFreshnessMiss bool                      `json:"expect_monitor_freshness_miss"`
	ExpectHardBlock            bool                      `json:"expect_hard_block,omitempty"`
	RequireMonitorIntent       bool                      `json:"require_monitor_intent,omitempty"`
}

type shadowFixtureTurn struct {
	UserMsg string `json:"user_msg"`
}

type shadowMonitorReport struct {
	MonitorFreshnessMiss bool
}

func loadShadowMonitorFixtures(t *testing.T, path string) []shadowMonitorFixture {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	out := make([]shadowMonitorFixture, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var fx shadowMonitorFixture
		require.NoError(t, json.Unmarshal([]byte(line), &fx), line)
		out = append(out, fx)
	}
	return out
}

func shadowPriorText(turns []shadowFixtureTurn) string {
	var b strings.Builder
	for _, turn := range turns {
		if strings.TrimSpace(turn.UserMsg) == "" {
			continue
		}
		b.WriteString("user: ")
		b.WriteString(turn.UserMsg)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

func shadowTraceRecordFromFixture(fx shadowMonitorFixture, planner observability.PlannerTrace) observability.TraceRecord {
	record := observability.TraceRecord{
		TurnIndex: 2,
		Planner:   planner,
	}
	if fx.ExpectHardBlock {
		record.EngineHardBlock = observability.EngineHardBlockTrace{
			Hit:      true,
			Category: "account_billing_unsupported",
		}
	}
	for i, action := range fx.ProductionToolActionsTurn2 {
		record.ToolCalls = append(record.ToolCalls, observability.ToolCallTrace{
			ID:        "tool-call-fixture",
			TurnIndex: 2,
			Action:    action,
			Source:    observability.ToolSourceMainReAct,
			ArgsHash:  "sha256:fixture",
			Status:    observability.ToolStatusSuccess,
			Attempts:  1 + i,
		})
	}
	record.Freshness.MonitorCallInCurrentTurn = hasCurrentTurnMonitorCall(record)
	return record
}

func evaluateShadowMonitorRecord(record observability.TraceRecord) shadowMonitorReport {
	return shadowMonitorReport{
		MonitorFreshnessMiss: isShadowMonitorIntent(record.Planner.Intent) && !hasCurrentTurnMonitorCall(record),
	}
}

func hasCurrentTurnMonitorCall(record observability.TraceRecord) bool {
	for _, call := range record.ToolCalls {
		if call.TurnIndex == record.TurnIndex && call.Action == "GetCompShareInstanceMonitor" {
			return true
		}
	}
	return false
}

func isShadowMonitorIntent(intent string) bool {
	return intent == string(intp.IntentMonitorQuery) || intent == string(intp.IntentMonitorHistory)
}

type shadowMonitorFixtureLLM struct{}

func (h *shadowMonitorFixtureLLM) CompleteIntentPlan(_ context.Context, req intp.PlannerLLMRequest) (string, error) {
	plan := classifyShadowMonitorFixture(req.UserPrompt)
	data, err := json.Marshal(plan)
	return string(data), err
}

func classifyShadowMonitorFixture(prompt string) intp.Plan {
	normalized := strings.ToLower(prompt)
	switch {
	case strings.Contains(normalized, "account balance"):
		return intp.Plan{
			SchemaVersion: intp.SchemaVersion,
			Intent:        intp.IntentBillingAccountUnsupported,
			Retrieval:     intp.Retrieval{Enabled: false},
			HardBlockHint: true,
			Confidence:    0.9,
		}
	case strings.Contains(normalized, "instance charge"):
		return shadowFixturePlan(intp.IntentBillingInstance, nil)
	case strings.Contains(normalized, "ssh"):
		return shadowFixturePlan(intp.IntentMixedDiagnosisKB, nil)
	case strings.Contains(normalized, "shutdown"):
		return shadowFixturePlan(intp.IntentOperationLifecycle, nil)
	case strings.Contains(normalized, "4090 vram size"):
		return shadowFixturePlan(intp.IntentKnowledgeQA, nil)
	case looksLikeMonitorFollowup(normalized):
		return shadowFixturePlan(intp.IntentMonitorQuery, []intp.Metric{intp.MetricGPU, intp.MetricVRAM})
	default:
		return shadowFixturePlan(intp.IntentUnknown, nil)
	}
}

func looksLikeMonitorFollowup(s string) bool {
	hasMetric := strings.Contains(s, "gpu") || strings.Contains(s, "vram")
	hasMonitorShape := strings.Contains(s, "monitor") ||
		strings.Contains(s, "refresh") ||
		strings.Contains(s, "right now") ||
		strings.Contains(s, "current") ||
		strings.Contains(s, "same machine")
	return hasMetric && hasMonitorShape
}

func shadowFixturePlan(intent intp.Intent, metrics []intp.Metric) intp.Plan {
	plan := intp.Plan{
		SchemaVersion: intp.SchemaVersion,
		Intent:        intent,
		Retrieval:     intp.Retrieval{Enabled: false},
		Confidence:    0.86,
	}
	if len(metrics) > 0 {
		plan.Slots.Metrics = metrics
		plan.Slots.TargetRefs = []intp.TargetRef{{
			Type:  intp.TargetRefFilter,
			Value: "all_running",
		}}
		plan.Slots.TimeWindow = &intp.TimeWindow{
			Type:  intp.TimeWindowPreset,
			Value: "last_60s",
		}
		plan.RequiredTools = []string{"DescribeCompShareInstance", "GetCompShareInstanceMonitor"}
	}
	return plan
}
