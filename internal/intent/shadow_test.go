package intent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShadowRunner_DisabledDoesNotCallPlanner(t *testing.T) {
	planner := &mockShadowPlanner{}
	runner := NewShadowRunner(planner, ShadowRunnerOptions{
		Enabled: false,
		Model:   "deepseek-v4-flash",
	})

	trace := runner.Run(context.Background(), PlannerInput{UserText: "monitor"})

	assert.False(t, trace.Enabled)
	assert.Zero(t, planner.calls)
	assert.Empty(t, trace.Intent)
	assert.False(t, trace.SchemaValid)
}

func TestShadowRunner_ShadowModeCallsPlannerOnce(t *testing.T) {
	now := steppedClock(time.Unix(100, 0), 42*time.Millisecond)
	planner := &mockShadowPlanner{
		result: PlannerResult{
			Plan: Plan{
				SchemaVersion: SchemaVersion,
				Intent:        IntentMonitorQuery,
				Slots:         Slots{Metrics: []Metric{MetricGPU}},
				Confidence:    0.8,
			},
		},
	}
	runner := NewShadowRunner(planner, ShadowRunnerOptions{
		Enabled: true,
		Model:   "deepseek-v4-flash",
		Now:     now,
	})

	input := PlannerInput{UserText: "看 GPU 监控"}
	trace := runner.Run(context.Background(), input)

	require.Equal(t, 1, planner.calls)
	assert.Equal(t, input, planner.lastInput)
	assert.True(t, trace.Enabled)
	assert.True(t, trace.SchemaValid)
	assert.Equal(t, "deepseek-v4-flash", trace.Model)
	assert.Equal(t, string(IntentMonitorQuery), trace.Intent)
	assert.Equal(t, int64(42), trace.LatencyMS)
	assert.Equal(t, []string{"gpu"}, trace.Slots.Metrics)
}

func TestShadowRunner_PlannerErrorReturnsInvalidTrace(t *testing.T) {
	planner := &mockShadowPlanner{err: errors.New("llm timeout")}
	runner := NewShadowRunner(planner, ShadowRunnerOptions{
		Enabled: true,
		Model:   "deepseek-v4-flash",
	})

	trace := runner.Run(context.Background(), PlannerInput{UserText: "monitor"})

	assert.Equal(t, 1, planner.calls)
	assert.True(t, trace.Enabled)
	assert.False(t, trace.SchemaValid)
	assert.Equal(t, string(IntentUnknown), trace.Intent)
	assert.Zero(t, trace.Confidence)
	assert.False(t, trace.HardBlockHint)
}

func TestShadowRunner_NilPlannerReturnsFallbackTrace(t *testing.T) {
	runner := NewShadowRunner(nil, ShadowRunnerOptions{
		Enabled: true,
		Model:   "deepseek-v4-flash",
	})

	trace := runner.Run(context.Background(), PlannerInput{UserText: "monitor"})

	assert.True(t, trace.Enabled)
	assert.False(t, trace.SchemaValid)
	assert.Equal(t, string(IntentUnknown), trace.Intent)
	assert.Zero(t, trace.Confidence)
	assert.False(t, trace.HardBlockHint)
}

func TestShadowRunner_FallbackResultReturnsInvalidTrace(t *testing.T) {
	planner := &mockShadowPlanner{
		result: PlannerResult{
			Fallback: true,
			Plan: Plan{
				SchemaVersion: SchemaVersion,
				Intent:        IntentBillingAccountUnsupported,
				HardBlockHint: true,
				Confidence:    0.75,
			},
		},
	}
	runner := NewShadowRunner(planner, ShadowRunnerOptions{
		Enabled: true,
		Model:   "deepseek-v4-flash",
	})

	trace := runner.Run(context.Background(), PlannerInput{UserText: "账号余额还有多少"})

	assert.Equal(t, 1, planner.calls)
	assert.True(t, trace.Enabled)
	assert.False(t, trace.SchemaValid)
	assert.Equal(t, string(IntentUnknown), trace.Intent)
	assert.Zero(t, trace.Confidence)
	assert.True(t, trace.HardBlockHint)
}

type mockShadowPlanner struct {
	calls     int
	lastInput PlannerInput
	result    PlannerResult
	err       error
}

func (m *mockShadowPlanner) Plan(_ context.Context, input PlannerInput) (PlannerResult, error) {
	m.calls++
	m.lastInput = input
	return m.result, m.err
}

func steppedClock(start time.Time, step time.Duration) func() time.Time {
	calls := 0
	return func() time.Time {
		t := start.Add(time.Duration(calls) * step)
		calls++
		return t
	}
}
