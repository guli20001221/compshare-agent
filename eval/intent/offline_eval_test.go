package intent_eval

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/entity"
	intp "github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Real-model intent-router eval flags. Default empty/0 → TestOnlineRouterEval skips,
// so the default `go test ./...` suite still runs only the deterministic mock above.
var (
	onlineModelFlag    = flag.String("model", "", "run the intent router against a real model (e.g. deepseek-v4-flash); empty = skip")
	onlineMinIntentAcc = flag.Float64("min-intent-acc", 0, "fail if real-model intent accuracy (%) is below this; 0 = report-only")
)

// onlineRouterLLM wraps a real llm.Client as the intent-router LLM (mirrors the
// production cliPlannerLLM in cmd/cli.go). responseFormat, when non-nil, applies
// the same structured-output request the production planner would send — set by
// onlineRouterResponseFormatFromEnv so the A/B can compare off vs json_schema.
type onlineRouterLLM struct {
	client         *llm.Client
	responseFormat *openai.ChatCompletionResponseFormat
}

func (o onlineRouterLLM) CompleteIntentPlan(ctx context.Context, req intp.IntentRouterLLMRequest) (string, error) {
	resp, err := o.client.Chat(ctx, llm.ChatRequest{
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: req.SystemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: req.UserPrompt},
		},
		ResponseFormat: o.responseFormat,
	})
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// onlineRouterResponseFormatFromEnv mirrors cmd/cli.go's
// plannerResponseFormatForMode so the eval exercises the SAME structured-output
// request production would send for the model under test. Reads
// COMPSHARE_INTENT_ROUTER_STRUCTURED_OUTPUT; nil when off (the shipped default)
// or when the model capability does not support the requested mode.
func onlineRouterResponseFormatFromEnv(baseURL, model string) *openai.ChatCompletionResponseFormat {
	mode := intp.SelectOutputMode(llm.LookupCapability(baseURL, model))
	switch strings.ToLower(strings.TrimSpace(os.Getenv("COMPSHARE_INTENT_ROUTER_STRUCTURED_OUTPUT"))) {
	case "json_schema":
		if mode == intp.OutputModeJSONSchema {
			return &openai.ChatCompletionResponseFormat{
				Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
				JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
					Name:   "intent_route",
					Schema: intp.IntentRouteResponseSchema(),
					Strict: false,
				},
			}
		}
		if mode == intp.OutputModeJSONObject {
			return &openai.ChatCompletionResponseFormat{Type: openai.ChatCompletionResponseFormatTypeJSONObject}
		}
	case "json_object":
		if mode == intp.OutputModeJSONObject || mode == intp.OutputModeJSONSchema {
			return &openai.ChatCompletionResponseFormat{Type: openai.ChatCompletionResponseFormatTypeJSONObject}
		}
	}
	return nil
}

// TestOnlineRouterEval runs the SAME fixtures through the REAL intent router instead
// of the keyword mock — closing the P0 where the only default "accuracy" gate scored
// a mock against fixtures it encodes. Gated on -model so the default suite skips it.
func TestOnlineRouterEval(t *testing.T) {
	if *onlineModelFlag == "" {
		t.Skip("use -model to evaluate the real intent router (e.g. -model deepseek-v4-flash)")
	}
	apiKey := os.Getenv("LLM_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("MODELVERSE_API_KEY")
	}
	if apiKey == "" {
		t.Fatal("-model set but no LLM_API_KEY / MODELVERSE_API_KEY in env")
	}
	baseURL := os.Getenv("LLM_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.modelverse.cn/v1"
	}

	client := llm.NewClient(config.LLMConfig{BaseURL: baseURL, APIKey: apiKey, Model: *onlineModelFlag})
	respFormat := onlineRouterResponseFormatFromEnv(baseURL, *onlineModelFlag)
	t.Logf("structured-output: env=%q applied=%v", os.Getenv("COMPSHARE_INTENT_ROUTER_STRUCTURED_OUTPUT"), respFormat != nil)
	router := intp.NewIntentRouter(onlineRouterLLM{client: client, responseFormat: respFormat}, intp.IntentRouterOptions{
		BaseURL: baseURL,
		Model:   *onlineModelFlag,
	})

	fixtures := loadFixtures(t, "fixtures.jsonl")
	require.GreaterOrEqual(t, len(fixtures), 50)

	var total, correct int
	for _, fx := range fixtures {
		reg := registryFromFixture(t, fx.RegistrySnapshot)
		result, err := router.Plan(context.Background(), intp.IntentRouterInput{
			UserText: fx.UserMsg,
			Registry: reg,
		})
		total++
		if err != nil {
			t.Logf("ERROR %s: %v", fx.ID, err)
			continue
		}
		if result.Plan.Intent == fx.ExpectedPlan.Intent {
			correct++
		} else {
			t.Logf("MISS %s: expected %s got %s", fx.ID, fx.ExpectedPlan.Intent, result.Plan.Intent)
		}
	}
	acc := float64(correct) / float64(total) * 100
	t.Logf("online intent router (%s): intent_accuracy=%.1f%% (%d/%d)", *onlineModelFlag, acc, correct, total)
	if *onlineMinIntentAcc > 0 && acc < *onlineMinIntentAcc {
		t.Errorf("intent accuracy %.1f%% below threshold %.1f%%", acc, *onlineMinIntentAcc)
	}
}

func TestOfflineFixturesEval(t *testing.T) {
	fixtures := loadFixtures(t, "fixtures.jsonl")
	require.GreaterOrEqual(t, len(fixtures), 50)

	planner := intp.NewIntentRouter(&heuristicFixtureLLM{}, intp.IntentRouterOptions{
		BaseURL: "https://api.modelverse.cn/v1",
		Model:   "Qwen/Qwen3-Max",
	})

	var legal, targetTotal, targetCorrect, unknownTotal, unknownCorrect int
	for _, fx := range fixtures {
		reg := registryFromFixture(t, fx.RegistrySnapshot)
		result, err := planner.Plan(context.Background(), intp.IntentRouterInput{
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
		if fx.ExpectedPlan.Intent == intp.IntentUnknown {
			unknownTotal++
			if result.Plan.Intent == intp.IntentUnknown {
				unknownCorrect++
			}
		}
		if len(fx.ExpectedPlan.TargetRefs) > 0 {
			assert.Equal(t, fx.ExpectedPlan.TargetRefs, result.Plan.Slots.TargetRefs, fx.ID)
		}
	}

	legalRate := float64(legal) / float64(len(fixtures))
	targetAccuracy := float64(targetCorrect) / float64(targetTotal)
	unknownAccuracy := float64(unknownCorrect) / float64(unknownTotal)
	t.Logf("intent offline eval: fixtures=%d legal_rate=%.2f target_accuracy=%.2f target_correct=%d/%d unknown_accuracy=%.2f unknown_correct=%d/%d",
		len(fixtures), legalRate, targetAccuracy, targetCorrect, targetTotal, unknownAccuracy, unknownCorrect, unknownTotal)
	assert.GreaterOrEqual(t, legalRate, 0.95)
	assert.GreaterOrEqual(t, targetAccuracy, 0.90)
	assert.GreaterOrEqual(t, unknownAccuracy, 0.90)
}

func TestExtractUserMessageUsesPlannerPromptLabel(t *testing.T) {
	prompt := "User question: SSH cannot connect\nPrior turns: assistant: prior answer\n"

	assert.Equal(t, "SSH cannot connect", extractUserMessage(prompt))
}

type fixture struct {
	ID               string                    `json:"id"`
	UserMsg          string                    `json:"user_msg"`
	RegistrySnapshot []entity.InstanceSnapshot `json:"registry_snapshot"`
	ExpectedPlan     expectedPlan              `json:"expected_plan"`
}

type expectedPlan struct {
	Intent        intp.Intent      `json:"intent"`
	RequiredTools []string         `json:"required_tools"`
	TargetRefs    []intp.TargetRef `json:"target_refs,omitempty"`
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

func (h *heuristicFixtureLLM) CompleteIntentPlan(_ context.Context, req intp.IntentRouterLLMRequest) (string, error) {
	msg := extractUserMessage(req.UserPrompt)
	plan := classifyFixtureMessage(msg)
	data, err := json.Marshal(plan)
	return string(data), err
}

func classifyFixtureMessage(msg string) intp.IntentRoute {
	normalized := strings.ToLower(msg)
	switch {
	case isResourceFilterText(normalized):
		return resourceFilterFixturePlan(normalized)
	case isMixedOrNonTargetText(normalized):
		return unknownFixturePlan()
	case strings.Contains(normalized, "uhost-abc123") && isMonitorText(normalized):
		return intp.IntentRoute{
			SchemaVersion: intp.SchemaVersion,
			Intent:        intp.IntentMonitorQuery,
			Slots: intp.Slots{
				TargetRefs: []intp.TargetRef{{
					Type:       intp.TargetRefUHostIDUserInput,
					Value:      "uhost-abc123",
					Source:     intp.SourceUserText,
					SourceSpan: "uhost-abc123",
				}},
				Metrics: []intp.Metric{intp.MetricCPU, intp.MetricGPU},
				TimeWindow: &intp.TimeWindow{
					Type:  intp.TimeWindowPreset,
					Value: "last_60s",
				},
			},
			RequiredTools: []string{"DescribeCompShareInstance", "GetCompShareInstanceMonitor"},
			Retrieval:     intp.Retrieval{Enabled: false},
			Confidence:    0.9,
		}
	case strings.Contains(normalized, "uhost-abc123") && isBillingInstanceText(normalized):
		return intp.IntentRoute{
			SchemaVersion: intp.SchemaVersion,
			Intent:        intp.IntentBillingInstance,
			Slots: intp.Slots{TargetRefs: []intp.TargetRef{{
				Type:       intp.TargetRefUHostIDUserInput,
				Value:      "uhost-abc123",
				Source:     intp.SourceUserText,
				SourceSpan: "uhost-abc123",
			}}},
			RequiredTools: []string{"DescribeCompShareInstance", "DiagnoseBilling"},
			Retrieval:     intp.Retrieval{Enabled: false},
			Confidence:    0.86,
		}
	case isAccountBillingUnsupportedText(normalized):
		return intp.IntentRoute{
			SchemaVersion: intp.SchemaVersion,
			Intent:        intp.IntentBillingAccountUnsupported,
			Retrieval:     intp.Retrieval{Enabled: false},
			HardBlockHint: true,
			Confidence:    0.9,
		}
	case isBillingInstanceText(normalized):
		return intp.IntentRoute{
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
	case isVagueFailureText(normalized):
		return vagueFailureFixturePlan()
	case isDiagnosisText(normalized):
		return diagnosisFixturePlan()
	case isMonitorText(normalized):
		return intp.IntentRoute{
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
		return unknownFixturePlan()
	}
}

func resourceFilterFixturePlan(normalized string) intp.IntentRoute {
	refs := []intp.TargetRef{}
	if strings.Contains(normalized, "在跑") ||
		strings.Contains(normalized, "运行") ||
		strings.Contains(normalized, "running") {
		refs = append(refs, intp.TargetRef{Type: intp.TargetRefFilter, Value: "state=running"})
	}
	if strings.Contains(normalized, "关机") ||
		strings.Contains(normalized, "停止") ||
		strings.Contains(normalized, "stopped") {
		refs = append(refs, intp.TargetRef{Type: intp.TargetRefFilter, Value: "state=stopped"})
	}
	if strings.Contains(normalized, "4090") {
		refs = append(refs, intp.TargetRef{Type: intp.TargetRefFilter, Value: "gpu_type=4090"})
	}
	return intp.IntentRoute{
		SchemaVersion: intp.SchemaVersion,
		Intent:        intp.IntentResourceInfo,
		Slots:         intp.Slots{TargetRefs: refs},
		RequiredTools: []string{"DescribeCompShareInstance"},
		Retrieval:     intp.Retrieval{Enabled: false},
		Confidence:    0.86,
	}
}

func unknownFixturePlan() intp.IntentRoute {
	return intp.IntentRoute{
		SchemaVersion: intp.SchemaVersion,
		Intent:        intp.IntentUnknown,
		Retrieval:     intp.Retrieval{Enabled: false},
		Confidence:    0.2,
	}
}

func diagnosisFixturePlan() intp.IntentRoute {
	return intp.IntentRoute{
		SchemaVersion: intp.SchemaVersion,
		Intent:        intp.IntentDiagnosis,
		Slots:         intp.Slots{},
		RequiredTools: []string{"DescribeCompShareInstance"},
		Retrieval:     intp.Retrieval{Enabled: false},
		Confidence:    0.84,
	}
}

func vagueFailureFixturePlan() intp.IntentRoute {
	return intp.IntentRoute{
		SchemaVersion: intp.SchemaVersion,
		Intent:        intp.IntentVagueFailure,
		Slots:         intp.Slots{},
		RequiredTools: []string{},
		Retrieval:     intp.Retrieval{Enabled: false},
		Confidence:    0.78,
	}
}

func extractUserMessage(prompt string) string {
	for _, marker := range []string{"User question:", "用户问题："} {
		idx := strings.Index(prompt, marker)
		if idx < 0 {
			continue
		}
		msg := prompt[idx+len(marker):]
		if next := strings.Index(msg, "\n"); next >= 0 {
			msg = msg[:next]
		}
		return strings.TrimSpace(msg)
	}
	return prompt
}

func isResourceFilterText(s string) bool {
	return strings.Contains(s, "哪些机器") ||
		strings.Contains(s, "哪些是") ||
		strings.Contains(s, "有哪些机器") ||
		(strings.Contains(s, "机器") && strings.Contains(s, "4090")) ||
		strings.Contains(s, "在跑") ||
		strings.Contains(s, "已经关机") ||
		strings.Contains(s, "running instances") ||
		strings.Contains(s, "stopped instances")
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

func isDiagnosisText(s string) bool {
	return strings.Contains(s, "ssh") ||
		strings.Contains(s, "连不上")
}

func isVagueFailureText(s string) bool {
	return strings.Contains(s, "跑崩") ||
		strings.Contains(s, "崩了") ||
		strings.Contains(s, "挂了")
}

func isMixedOrNonTargetText(s string) bool {
	hasMonitor := isMonitorText(s)
	hasBilling := isBillingInstanceText(s) ||
		strings.Contains(s, "费用") ||
		strings.Contains(s, "扣费")
	hasDiagnosis := strings.Contains(s, "连不上") ||
		strings.Contains(s, "诊断") ||
		strings.Contains(s, "跑崩") ||
		strings.Contains(s, "崩了")
	hasOperation := strings.Contains(s, "重启") ||
		strings.Contains(s, "关闭") ||
		strings.Contains(s, "开机")
	hasExpiry := strings.Contains(s, "到期") ||
		strings.Contains(s, "续费") ||
		strings.Contains(s, "自动续费")
	return hasExpiry ||
		hasOperation ||
		(hasMonitor && hasBilling) ||
		(hasDiagnosis && hasBilling)
}

func isTargetIntent(intent intp.Intent) bool {
	return intent == intp.IntentMonitorQuery ||
		intent == intp.IntentResourceInfo ||
		intent == intp.IntentBillingInstance ||
		intent == intp.IntentBillingAccountUnsupported ||
		intent == intp.IntentDiagnosis ||
		intent == intp.IntentVagueFailure
}
