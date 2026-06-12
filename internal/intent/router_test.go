package intent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/compshare-agent/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelectOutputMode_ThinkingModeChoosesJSONObjectBeforeJSONSchema(t *testing.T) {
	mode := SelectOutputMode(llm.Capability{
		SupportsJSONSchema: true,
		SupportsJSONObject: true,
		IsThinkingMode:     true,
	})

	assert.Equal(t, OutputModeJSONObject, mode)
}

func TestPlanner_ReturnsValidPlanFromMockLLM(t *testing.T) {
	mock := &mockPlannerLLM{responses: []string{mustPlanJSON(t, validMonitorPlan())}}
	planner := NewIntentRouter(mock, IntentRouterOptions{
		BaseURL: "https://api.modelverse.cn/v1",
		Model:   "Qwen/Qwen3-Max",
	})

	result, err := planner.Plan(context.Background(), IntentRouterInput{
		UserText: "看看 uhost-abc123 的 CPU 和 GPU 监控",
		Registry: testRegistry(t),
	})

	require.NoError(t, err)
	assert.False(t, result.Fallback)
	assert.Equal(t, IntentMonitorQuery, result.Plan.Intent)
	assert.Equal(t, 1, result.Attempts)
	assert.Equal(t, OutputModeJSONObject, result.Mode)
	require.Len(t, mock.requests, 1)
	assert.Contains(t, mock.requests[0].SystemPrompt, "monitor_query")
}

func TestPlanner_ValidatesAgainstImmutableResolverSnapshot(t *testing.T) {
	reg := testRegistry(t)
	snap := reg.Snapshot()
	require.NoError(t, reg.SyncFromDescribe(map[string]any{
		"TotalCount": float64(0),
		"UHostSet":   []any{},
	}, "test"))

	mock := &mockPlannerLLM{responses: []string{mustPlanJSON(t, validMonitorPlan())}}
	planner := NewIntentRouter(mock, IntentRouterOptions{})

	result, err := planner.Plan(context.Background(), IntentRouterInput{
		UserText: "check uhost-abc123 monitor",
		Resolver: snap,
	})

	require.NoError(t, err)
	assert.False(t, result.Fallback)
	assert.Equal(t, IntentMonitorQuery, result.Plan.Intent)
}

func TestPlanner_RetriesInvalidJSONThenReturnsValidPlan(t *testing.T) {
	mock := &mockPlannerLLM{responses: []string{
		`{"schema_version":`,
		mustPlanJSON(t, validMonitorPlan()),
	}}
	planner := NewIntentRouter(mock, IntentRouterOptions{
		BaseURL: "https://unknown.example/v1",
		Model:   "unknown",
	})

	result, err := planner.Plan(context.Background(), IntentRouterInput{
		UserText: "看看 uhost-abc123 的 CPU 和 GPU 监控",
		Registry: testRegistry(t),
	})

	require.NoError(t, err)
	assert.False(t, result.Fallback)
	assert.Equal(t, 2, result.Attempts)
	assert.Equal(t, OutputModeStrictPromptJSON, result.Mode)
	require.Len(t, mock.requests, 2)
	assert.Contains(t, mock.requests[1].UserPrompt, "上一轮输出不是合法 IntentPlan JSON")
}

func TestPlanner_RetryInstructionNamesValidationCodeAndField(t *testing.T) {
	badPlan := validMonitorPlan()
	badPlan.RequiredTools = []string{"DescribeCompShareImages"}
	mock := &mockPlannerLLM{responses: []string{
		mustPlanJSON(t, badPlan),
		mustPlanJSON(t, validMonitorPlan()),
	}}
	planner := NewIntentRouter(mock, IntentRouterOptions{})

	result, err := planner.Plan(context.Background(), IntentRouterInput{
		UserText: "看看 uhost-abc123 的 CPU 和 GPU 监控",
		Registry: testRegistry(t),
	})

	require.NoError(t, err)
	assert.False(t, result.Fallback)
	assert.Equal(t, 2, result.Attempts)
	require.Len(t, mock.requests, 2)
	assert.Contains(t, mock.requests[1].UserPrompt, string(ErrInvalidRequiredTool))
	assert.Contains(t, mock.requests[1].UserPrompt, "required_tools[0]")
	assert.Contains(t, mock.requests[1].UserPrompt, "required_tools 必须匹配 intent 的工具白名单")
	assert.NotContains(t, mock.requests[1].UserPrompt, "上一轮输出不是合法 IntentPlan JSON")
}

func TestPlanner_OverridesLLMSuppliedSkillsWithDerivedProjection(t *testing.T) {
	plan := IntentRoute{
		SchemaVersion: SchemaVersion,
		Intent:        IntentPricingQuery,
		Skills: []SelectedSkill{
			{Name: "deploy_model", Resolution: "planner_supplied"},
		},
		Slots:         Slots{},
		RequiredTools: []string{"GetCompShareInstancePrice"},
		Retrieval:     Retrieval{Enabled: false},
		Confidence:    0.9,
	}
	mock := &mockPlannerLLM{responses: []string{mustPlanJSON(t, plan)}}
	planner := NewIntentRouter(mock, IntentRouterOptions{})

	result, err := planner.Plan(context.Background(), IntentRouterInput{UserText: "4090 多少钱"})

	require.NoError(t, err)
	require.Len(t, result.Plan.Skills, 1)
	assert.Equal(t, SelectedSkill{Name: "pricing_query", Resolution: SkillResolutionDerivedFromIntent}, result.Plan.Skills[0])
}

func TestPlanner_FallsBackUnknownAfterInvalidPartialPlans(t *testing.T) {
	mock := &mockPlannerLLM{responses: []string{
		`{"intent":"monitor_query"}`,
		`{"intent":"monitor_query"}`,
	}}
	planner := NewIntentRouter(mock, IntentRouterOptions{})

	result, err := planner.Plan(context.Background(), IntentRouterInput{
		UserText: "看看 uhost-abc123 的 CPU 和 GPU 监控",
		Registry: testRegistry(t),
	})

	require.NoError(t, err)
	assert.True(t, result.Fallback)
	assert.Equal(t, IntentUnknown, result.Plan.Intent)
	assert.Equal(t, 2, result.Attempts)
	assert.Equal(t, ErrInvalidSchemaVersion, result.LastValidationCode)
}

func TestPlanner_ReturnsErrorWhenLLMCallFails(t *testing.T) {
	mock := &mockPlannerLLM{err: errors.New("llm unavailable")}
	planner := NewIntentRouter(mock, IntentRouterOptions{})

	result, err := planner.Plan(context.Background(), IntentRouterInput{
		UserText: "看看 uhost-abc123 的 CPU 和 GPU 监控",
		Registry: testRegistry(t),
	})

	require.Error(t, err)
	assert.True(t, result.Fallback)
	assert.Equal(t, IntentUnknown, result.Plan.Intent)
	assert.Equal(t, 1, result.Attempts)
}

type mockPlannerLLM struct {
	responses []string
	err       error
	requests  []IntentRouterLLMRequest
}

func (m *mockPlannerLLM) CompleteIntentPlan(_ context.Context, req IntentRouterLLMRequest) (string, error) {
	m.requests = append(m.requests, req)
	if m.err != nil {
		return "", m.err
	}
	if len(m.responses) == 0 {
		return "", nil
	}
	out := m.responses[0]
	m.responses = m.responses[1:]
	return out, nil
}

func mustPlanJSON(t *testing.T, plan IntentRoute) string {
	t.Helper()
	data, err := json.Marshal(plan)
	require.NoError(t, err)
	return string(data)
}
