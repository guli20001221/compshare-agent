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
	require.Contains(t, second.Observation, "reused_observation")
	require.Contains(t, second.Observation, "same_call_blocked")
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

	require.NotContains(t, first.Observation, "reused_observation")
	require.NotContains(t, second.Observation, "reused_observation")
	require.Len(t, executor.calls, 2, "不同参数即使结果相同也必须真正执行")
	require.Contains(t, toolNames(centralAgentToolWindow(false, false)), action)
}

func TestMonitorCallBudgetStopsThirdCall(t *testing.T) {
	executor := &mockExecutor{}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.toolResultsByCallThisTurn = map[string]string{}
	action := capability.ReadToolName(intent.IntentMonitorHistory)

	for index, args := range []string{
		`{"time_window":{"type":"relative","amount":1,"unit":"hour"}}`,
		`{"time_window":{"type":"relative","amount":2,"unit":"hour"}}`,
	} {
		result := execToolInTurn(eng, toolCall("call", action, args), noopStep)
		require.NotContains(t, result.Observation, "call_budget_exhausted", "call %d", index+1)
	}
	third := execToolInTurn(eng, toolCall("third", action,
		`{"time_window":{"type":"relative","amount":3,"unit":"hour"}}`), noopStep)
	require.Contains(t, third.Observation, "call_budget_exhausted")
	require.Contains(t, third.Observation, `"max_calls_per_turn":2`)
	require.Equal(t, 3, eng.agentToolCallsThisTurn(action),
		"the refused call is still a call the model spent a round on")
}

// The budget counts calls, not distinct arguments: a round the model spent
// re-asking with the same parameters cost the same as any other. Counting cached
// argument keys made a repeat free, so a capability could be re-asked past its
// budget by alternating between a repeat and a variation.
func TestMonitorCallBudgetCountsARepeatedCallToo(t *testing.T) {
	executor := &mockExecutor{}
	eng := NewWithDeps(&mockLLM{}, executor, nil)
	eng.toolResultsByCallThisTurn = map[string]string{}
	action := capability.ReadToolName(intent.IntentMonitorHistory)
	const sameArgs = `{"time_window":{"type":"relative","amount":1,"unit":"hour"}}`

	execToolInTurn(eng, toolCall("first", action, sameArgs), noopStep)
	repeat := execToolInTurn(eng, toolCall("second", action, sameArgs), noopStep)
	third := execToolInTurn(eng, toolCall("third", action,
		`{"time_window":{"type":"relative","amount":2,"unit":"hour"}}`), noopStep)

	require.Contains(t, repeat.Observation, "reused_observation", "an identical read replays its observation")
	require.Contains(t, third.Observation, "call_budget_exhausted",
		"the replayed round still spent one of the turn's calls")
}

func TestInstanceOpsCallBudgetStopsThirdRun(t *testing.T) {
	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "已核实"}}
	eng := newInstanceOpsEngine(runner, nil)
	eng.toolResultsByCallThisTurn = map[string]string{}

	for index, task := range []string{"检查服务", "复核服务"} {
		callID := []string{"guest-first", "guest-second"}[index]
		result := execToolInTurn(eng, toolCall(callID, "DiagnoseInstanceInternals",
			`{"UHostId":"uhost-1","Task":"`+task+`"}`), noopStep)
		require.NotContains(t, result.Observation, "call_budget_exhausted", "run %d", index+1)
	}
	third := execToolInTurn(eng, toolCall("guest-third", "DiagnoseInstanceInternals",
		`{"UHostId":"uhost-1","Task":"第三次检查"}`), noopStep)

	require.Contains(t, third.Observation, "call_budget_exhausted")
	require.Contains(t, third.Observation, `"max_calls_per_turn":2`)
	require.Equal(t, MaxInstanceOpsRunsPerTurn, runner.calls)
}

// A Guest run is never replayed from the reuse cache. A retry after a dropped
// connection re-enters the instance instead of being answered with the previous
// attempt's partial command list — the budget above is what bounds it.
func TestInstanceOpsRetryWithIdenticalArgumentsReentersTheGuest(t *testing.T) {
	runner := &fakeInstanceOpsRunner{err: context.Canceled, progress: []InstanceOpsProgress{{
		Kind: InstanceOpsProgressCommand, Command: "systemctl restart nvidia-persistenced",
		Tier: "mutating", Disposition: "ran",
	}}}
	eng := newInstanceOpsEngine(runner, nil)
	eng.toolResultsByCallThisTurn = map[string]string{}
	const args = `{"UHostId":"uhost-1","Task":"repair service"}`

	first := execToolInTurn(eng, toolCall("first", "DiagnoseInstanceInternals", args), noopStep)
	runner.progress = nil
	second := execToolInTurn(eng, toolCall("second", "DiagnoseInstanceInternals", args), noopStep)

	require.Equal(t, 2, runner.calls, "an identical-args retry must reach the runner")
	require.NotContains(t, second.Observation, "reused_observation")
	require.Contains(t, first.Observation, "systemctl restart nvidia-persistenced")
	require.NotContains(t, second.Observation, "systemctl restart nvidia-persistenced",
		"the retry must report its own run, not the previous attempt's commands")
}

func TestZoneCatalogIsSingleShotOnlyAfterSuccessfulObservation(t *testing.T) {
	action := capability.ReadToolName(intent.IntentZoneCatalog)
	results := map[string]string{
		toolProgressCallKey(action, map[string]any{"query": "invented"}): `{"status":"fallback_before_tool"}`,
	}
	require.False(t, completedAgentToolCall(results, action), "a validation failure must leave room for one corrected call")

	results[toolProgressCallKey(action, map[string]any{})] = `{"status":"handled"}`
	require.True(t, singleShotAgentTool(action))
	require.True(t, completedAgentToolCall(results, action))
}
