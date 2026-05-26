package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/refusal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImageContext_PlannerReceivesSeparateFields(t *testing.T) {
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{
		Plan: unknownEngineTestPlan(),
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		Model:          "test",
		EnabledIntents: []intent.Intent{intent.IntentResourceInfo},
	})

	_, err := eng.ChatWithOptions(context.Background(), "帮我看看", noopStep, ChatOptions{
		ImageContext: "页面/场景：实例监控页\n可见文字：GPU 使用率 0%\n错误/异常：CUDA OOM",
	})
	require.NoError(t, err)
	require.Len(t, planner.calls, 1)

	assert.Equal(t, "帮我看看", planner.calls[0].UserText,
		"planner UserText must be raw user question, not enriched")
	assert.Contains(t, planner.calls[0].ImageContext, "CUDA OOM",
		"planner ImageContext must carry screenshot summary")
	assert.NotContains(t, planner.calls[0].UserText, "CUDA",
		"screenshot content must not leak into UserText")
}

func TestImageContext_PlannerMonitorHistoryOverriddenByCodeGuardrail(t *testing.T) {
	// Planner returns monitor_history (simulating misclassification from
	// screenshot UI labels like "运维监控" + "最近访问"). Code-level guardrail
	// in tryPlannerDispatch checks: image context present + raw userMsg
	// does NOT match isUnsupportedHistoricalMonitorQuestion → override
	// the planner, fall through to ReAct instead of refusing.
	planner := &scriptedIntentPlanner{results: []intent.PlannerResult{{
		Plan: intent.Plan{
			SchemaVersion: intent.SchemaVersion,
			Intent:        intent.IntentMonitorHistory,
			Slots:         intent.Slots{},
			Retrieval:     intent.Retrieval{Enabled: false},
			Confidence:    0.9,
		},
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "ReAct 正常回答"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test user")
	eng.SetIntentPlanner(planner, IntentPlannerOptions{
		Model:          "test",
		EnabledIntents: []intent.Intent{intent.IntentResourceInfo, intent.IntentMonitorQuery},
	})

	reply, err := eng.ChatWithOptions(context.Background(), "帮我看看", noopStep, ChatOptions{
		ImageContext: "页面/场景：运维监控\n可见文字：最近访问、昨天CPU利用率80%",
	})
	require.NoError(t, err)

	// Code guardrail overrides planner: raw "帮我看看" has no monitor
	// keywords, so monitor_history refusal is suppressed. Engine falls
	// through to ReAct, which calls the mock LLM.
	assert.NotEqual(t, refusal.MonitorHistoryUnsupported, reply,
		"monitor_history refusal must be suppressed when image context is present and raw userMsg is benign")
	assert.Equal(t, "ReAct 正常回答", reply)
	assert.Len(t, mock.calls, 1, "ReAct LLM should be called after guardrail override")

	// Verify planner got ImageContext separately.
	require.Len(t, planner.calls, 1)
	assert.Equal(t, "帮我看看", planner.calls[0].UserText)
	assert.Contains(t, planner.calls[0].ImageContext, "运维监控")
}
