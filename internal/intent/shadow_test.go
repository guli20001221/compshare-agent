package intent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/compshare-agent/internal/governance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShadowRunner_DisabledDoesNotCallPlanner(t *testing.T) {
	planner := &mockShadowPlanner{}
	runner := NewShadowRunner(planner, ShadowRunnerOptions{
		Enabled: false,
		Model:   "deepseek-v4-flash",
	})

	trace := runner.Run(context.Background(), IntentRouterInput{UserText: "monitor"})

	assert.False(t, trace.Enabled)
	assert.Zero(t, planner.calls)
	assert.Empty(t, trace.Intent)
	assert.False(t, trace.SchemaValid)
}

func TestShadowRunner_ShadowModeCallsPlannerOnce(t *testing.T) {
	now := steppedClock(time.Unix(100, 0), 42*time.Millisecond)
	planner := &mockShadowPlanner{
		result: IntentRouterResult{
			Plan: IntentRoute{
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

	input := IntentRouterInput{UserText: "看 GPU 监控"}
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

	trace := runner.Run(context.Background(), IntentRouterInput{UserText: "monitor"})

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

	trace := runner.Run(context.Background(), IntentRouterInput{UserText: "monitor"})

	assert.True(t, trace.Enabled)
	assert.False(t, trace.SchemaValid)
	assert.Equal(t, string(IntentUnknown), trace.Intent)
	assert.Zero(t, trace.Confidence)
	assert.False(t, trace.HardBlockHint)
}

func TestShadowRunner_FallbackResultReturnsInvalidTrace(t *testing.T) {
	planner := &mockShadowPlanner{
		result: IntentRouterResult{
			Fallback: true,
			Plan: IntentRoute{
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

	trace := runner.Run(context.Background(), IntentRouterInput{UserText: "账号余额还有多少"})

	assert.Equal(t, 1, planner.calls)
	assert.True(t, trace.Enabled)
	assert.False(t, trace.SchemaValid)
	assert.Equal(t, string(IntentUnknown), trace.Intent)
	assert.Zero(t, trace.Confidence)
	assert.False(t, trace.HardBlockHint)
}

func TestShadowRunner_QuotaDenialSkipsPlannerAndReturnsInvalidTrace(t *testing.T) {
	planner := &mockShadowPlanner{}
	// Production callers pass a hashed subject. This intentionally raw test
	// value verifies quota metadata cannot leak through RouterTrace.
	rawSubject := "raw-public-key-must-not-appear"
	var requests []governance.Request
	runner := NewShadowRunner(planner, ShadowRunnerOptions{
		Enabled:      true,
		Model:        "deepseek-v4-flash",
		QuotaSubject: rawSubject,
		QuotaHook: func(req governance.Request) governance.Decision {
			requests = append(requests, req)
			return governance.Decision{
				Allowed:     false,
				Class:       req.Class,
				Action:      req.Action,
				Reason:      governance.ReasonQPSExceeded,
				SubjectHash: req.SubjectKey,
				Err:         governance.ErrRateLimited,
			}
		},
	})

	trace := runner.Run(context.Background(), IntentRouterInput{UserText: "monitor"})

	require.Len(t, requests, 1)
	assert.Equal(t, governance.ClassLLM, requests[0].Class)
	assert.Equal(t, "shadow_planner", requests[0].Action)
	assert.Equal(t, rawSubject, requests[0].SubjectKey)
	assert.Zero(t, planner.calls, "quota denial must skip planner LLM call")
	assert.True(t, trace.Enabled)
	assert.False(t, trace.SchemaValid)
	assert.Equal(t, string(IntentUnknown), trace.Intent)
	assert.Zero(t, trace.Confidence)
	data, err := json.Marshal(trace)
	require.NoError(t, err)
	assert.NotContains(t, string(data), rawSubject)
}

func TestShadowRunner_QuotaAllowCallsPlannerOnce(t *testing.T) {
	planner := &mockShadowPlanner{
		result: IntentRouterResult{
			Plan: IntentRoute{
				SchemaVersion: SchemaVersion,
				Intent:        IntentMonitorQuery,
				Slots:         Slots{Metrics: []Metric{MetricGPU}},
				Confidence:    0.8,
			},
		},
	}
	var requests []governance.Request
	runner := NewShadowRunner(planner, ShadowRunnerOptions{
		Enabled: true,
		Model:   "deepseek-v4-flash",
		QuotaHook: func(req governance.Request) governance.Decision {
			requests = append(requests, req)
			return governance.Decision{Allowed: true, Class: req.Class, Action: req.Action, SubjectHash: req.SubjectKey}
		},
	})

	trace := runner.Run(context.Background(), IntentRouterInput{UserText: "monitor"})

	require.Len(t, requests, 1)
	assert.Equal(t, governance.ClassLLM, requests[0].Class)
	assert.Equal(t, "shadow_planner", requests[0].Action)
	assert.Equal(t, 1, planner.calls)
	assert.True(t, trace.SchemaValid)
	assert.Equal(t, string(IntentMonitorQuery), trace.Intent)
}

func TestShadowRunner_NilQuotaHookCallsPlannerNormally(t *testing.T) {
	planner := &mockShadowPlanner{
		result: IntentRouterResult{
			Plan: IntentRoute{
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
	})

	trace := runner.Run(context.Background(), IntentRouterInput{UserText: "monitor"})

	assert.Equal(t, 1, planner.calls)
	assert.True(t, trace.Enabled)
	assert.True(t, trace.SchemaValid)
	assert.Equal(t, string(IntentMonitorQuery), trace.Intent)
}

func TestShadowRunner_DisabledDoesNotCallQuotaHook(t *testing.T) {
	planner := &mockShadowPlanner{}
	quotaCalls := 0
	runner := NewShadowRunner(planner, ShadowRunnerOptions{
		Enabled: false,
		Model:   "deepseek-v4-flash",
		QuotaHook: func(req governance.Request) governance.Decision {
			quotaCalls++
			return governance.Decision{Allowed: true}
		},
	})

	trace := runner.Run(context.Background(), IntentRouterInput{UserText: "monitor"})

	assert.False(t, trace.Enabled)
	assert.Zero(t, quotaCalls)
	assert.Zero(t, planner.calls)
}

type mockShadowPlanner struct {
	calls     int
	lastInput IntentRouterInput
	result    IntentRouterResult
	err       error
}

func (m *mockShadowPlanner) Plan(_ context.Context, input IntentRouterInput) (IntentRouterResult, error) {
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
