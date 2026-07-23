package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/intent"
	"github.com/stretchr/testify/require"
)

func TestRepeatedConcreteReadReusesOnlyTheIdenticalCall(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeAvailableCompShareInstanceTypes": {"InstanceTypes": []any{map[string]any{"GpuType": "4090"}}},
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.toolResultsByCallThisTurn = map[string]string{}
	action := capability.ReadToolName(intent.IntentGPUSpecsQuery)
	call := toolCall("read", action, `{"gpu_type":"4090"}`)

	first := eng.executeTool(context.Background(), call, noopStep)
	second := eng.executeTool(context.Background(), call, noopStep)

	require.NotEqual(t, first, second)
	require.Contains(t, second, "reused_observation")
	require.Contains(t, second, "same_call_blocked")
	require.Len(t, executor.calls, 1)
	require.Contains(t, toolNames(centralAgentToolWindow(false, false)), action, "复用一次调用不能撤掉整个能力")
}

func TestDifferentArgumentsRemainExecutableWhenResultsMatch(t *testing.T) {
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeAvailableCompShareInstanceTypes": {"InstanceTypes": []any{}},
	}}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.toolResultsByCallThisTurn = map[string]string{}
	action := capability.ReadToolName(intent.IntentGPUSpecsQuery)

	first := eng.executeTool(context.Background(), toolCall("first", action, `{"gpu_type":"4090"}`), noopStep)
	second := eng.executeTool(context.Background(), toolCall("second", action, `{"gpu_type":"A100"}`), noopStep)

	require.NotContains(t, first, "reused_observation")
	require.NotContains(t, second, "reused_observation")
	require.Len(t, executor.calls, 2, "不同参数即使结果相同也必须真正执行")
	require.Contains(t, toolNames(centralAgentToolWindow(false, false)), action)
}

func TestMonitorUniqueCallBudgetStopsThirdVariant(t *testing.T) {
	executor := &mockExecutor{}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.toolResultsByCallThisTurn = map[string]string{}
	action := capability.ReadToolName(intent.IntentMonitorHistory)

	for index, args := range []string{
		`{"time_window":{"type":"relative","amount":1,"unit":"hour"}}`,
		`{"time_window":{"type":"relative","amount":2,"unit":"hour"}}`,
	} {
		result := eng.executeTool(context.Background(), toolCall("call", action, args), noopStep)
		require.NotContains(t, result, "call_budget_exhausted", "call %d", index+1)
	}
	third := eng.executeTool(context.Background(), toolCall("third", action,
		`{"time_window":{"type":"relative","amount":3,"unit":"hour"}}`), noopStep)
	require.Contains(t, third, "call_budget_exhausted")
	require.Equal(t, 2, uniqueAgentToolCalls(eng.toolResultsByCallThisTurn, action))
}
