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
	planner := NewPlanner(mock, PlannerOptions{
		BaseURL: "https://api.modelverse.cn/v1",
		Model:   "Qwen/Qwen3-Max",
	})

	result, err := planner.Plan(context.Background(), PlannerInput{
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

func TestPlanner_RetriesInvalidJSONThenReturnsValidPlan(t *testing.T) {
	mock := &mockPlannerLLM{responses: []string{
		`{"schema_version":`,
		mustPlanJSON(t, validMonitorPlan()),
	}}
	planner := NewPlanner(mock, PlannerOptions{
		BaseURL: "https://unknown.example/v1",
		Model:   "unknown",
	})

	result, err := planner.Plan(context.Background(), PlannerInput{
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

func TestPlanner_FallsBackUnknownAfterInvalidPartialPlans(t *testing.T) {
	mock := &mockPlannerLLM{responses: []string{
		`{"intent":"monitor_query"}`,
		`{"intent":"monitor_query"}`,
	}}
	planner := NewPlanner(mock, PlannerOptions{})

	result, err := planner.Plan(context.Background(), PlannerInput{
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
	planner := NewPlanner(mock, PlannerOptions{})

	result, err := planner.Plan(context.Background(), PlannerInput{
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
	requests  []PlannerLLMRequest
}

func (m *mockPlannerLLM) CompleteIntentPlan(_ context.Context, req PlannerLLMRequest) (string, error) {
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

func mustPlanJSON(t *testing.T, plan Plan) string {
	t.Helper()
	data, err := json.Marshal(plan)
	require.NoError(t, err)
	return string(data)
}
