package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

// Exercise the real central loop, not just a direct dispatch call: two bound
// Guest runs separated by another tool must both execute before the final reply.
func TestInstanceOpsCanContinueAcrossOtherToolsInOneTurn(t *testing.T) {
	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "服务已监听，平台入口仍待验证", Ran: 2}}
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("guest-first", "DiagnoseInstanceInternals", `{"UHostId":"uhost-1","Task":"检查服务"}`)}},
		{ToolCalls: []openai.ToolCall{toolCall("other-facts", "SearchKnowledge", `{"queries":["平台入口验证"]}`)}},
		{ToolCalls: []openai.ToolCall{toolCall("guest-verify", "DiagnoseInstanceInternals", `{"UHostId":"uhost-1","Task":"验证平台入口"}`)}},
		{Content: "已完成检查，未验证项仍以实际报告为准。"},
	}}
	eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{}}, nil)
	eng.SetInstanceOps(runner)
	_, err := eng.Chat(context.Background(), "请检查 uhost-1 的服务并验证平台入口", noopStep)
	require.NoError(t, err)
	require.Equal(t, 2, runner.calls)
	require.Len(t, model.calls, 4)
	require.Equal(t, "guest-verify", runner.lastReq.InvocationID)
	var reportSeen bool
	for _, msg := range model.calls[1].Messages {
		if msg.Role != openai.ChatMessageRoleTool {
			continue
		}
		result, ok := tools.ParseAgentToolResult(msg.Content)
		if ok && result.Meta.Action == "DiagnoseInstanceInternals" {
			data := result.Data.(map[string]any)
			require.Equal(t, runner.verdict.Text, data["report"])
			require.Equal(t, float64(2), data["commands_ran"])
			reportSeen = true
		}
	}
	require.True(t, reportSeen)
}

type reportThenErrorLLM struct{ calls int }

func (m *reportThenErrorLLM) Chat(context.Context, llm.ChatRequest) (*llm.ChatResponse, error) {
	m.calls++
	if m.calls > 1 {
		return nil, errors.New("provider unavailable")
	}
	return &llm.ChatResponse{ToolCalls: []openai.ToolCall{toolCall("guest-done", "DiagnoseInstanceInternals", `{"UHostId":"uhost-1","Task":"修复服务"}`)}}, nil
}

func TestInstanceOpsReportSurvivesParentFailureWithoutRerunning(t *testing.T) {
	const report = "已重启原有服务；本地响应正常，公网入口尚未验证。"
	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: report, Ran: 3}}
	model := &reportThenErrorLLM{}
	eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{}}, nil)
	eng.SetInstanceOps(runner)
	reply, err := eng.Chat(context.Background(), "请修复 uhost-1 的服务", noopStep)
	require.NoError(t, err)
	require.Contains(t, reply, report)
	require.Contains(t, reply, "后续汇总未完成")
	require.NotContains(t, reply, "provider unavailable")
	require.Equal(t, 1, runner.calls)
	require.Equal(t, 2, model.calls)
}

func TestInterruptedInstanceOpsReportSurvivesParentFailureAndCanBeAcknowledged(t *testing.T) {
	runner := &fakeInstanceOpsRunner{
		err: errors.New("runner transport failed after command"),
		progress: []InstanceOpsProgress{{
			Kind: InstanceOpsProgressCommand, Command: "restart original service",
			Tier: "mutating", Disposition: "ran",
		}},
	}
	model := &reportThenErrorLLM{}
	eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{}}, nil)
	eng.SetInstanceOps(runner)

	reply, err := eng.Chat(context.Background(), "请修复 uhost-1 的服务", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "restart original service")
	require.Contains(t, reply, "后续汇总未完成")
	require.NotNil(t, eng.pendingInstanceOpsInterruption)
	require.True(t, eng.instanceOpsInterruptionIncludedInReplyThisTurn)
	eng.AcknowledgeDeliveredInstanceOpsInterruption()
	require.Nil(t, eng.pendingInstanceOpsInterruption,
		"a persisted parent-error recovery must not repeat on the next turn")
}

func TestInstanceOpsWallClockTimeoutDeliversSettledReportWithoutReentry(t *testing.T) {
	runner := &fakeInstanceOpsRunner{
		err: ErrInstanceOpsTimedOut,
		progress: []InstanceOpsProgress{{
			Kind: InstanceOpsProgressCommand, Command: "restart original service",
			Tier: "mutating", Disposition: "ran",
		}},
	}
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("guest-timeout", "DiagnoseInstanceInternals", `{"UHostId":"uhost-1","Task":"修复服务"}`)}},
		{ToolCalls: []openai.ToolCall{toolCall("guest-retry", "DiagnoseInstanceInternals", `{"UHostId":"uhost-1","Task":"再次修复服务"}`)}},
	}}
	eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{}}, nil)
	eng.SetInstanceOps(runner)

	reply, err := eng.Chat(context.Background(), "请修复 uhost-1 的服务", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "restart original service")
	require.Contains(t, reply, "后续汇总未完成")
	require.Equal(t, 1, runner.calls)
	require.Len(t, model.calls, 1)
	require.NotNil(t, eng.pendingInstanceOpsInterruption, "the engine cannot assume its reply crossed the transport boundary")
	require.True(t, eng.instanceOpsInterruptionIncludedInReplyThisTurn)
	eng.AcknowledgeDeliveredInstanceOpsInterruption()
	require.Nil(t, eng.pendingInstanceOpsInterruption, "a durably delivered timeout report must not repeat next turn")
}

func TestDeterministicToolReplyKeepsEarlierInstanceReport(t *testing.T) {
	runner := &fakeInstanceOpsRunner{
		err: errors.New("runner transport failed after command"),
		progress: []InstanceOpsProgress{{
			Kind: InstanceOpsProgressCommand, Command: "restart original service",
			Tier: "mutating", Disposition: "ran",
		}},
	}
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("guest-first", "DiagnoseInstanceInternals", `{"UHostId":"uhost-1","Task":"恢复服务"}`)}},
		customerSupportToolCall(),
	}}
	eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{}}, nil)
	eng.SetInstanceOps(runner)

	reply, err := eng.Chat(context.Background(), "先恢复 uhost-1；若不能继续再给我人工入口", noopStep)

	require.NoError(t, err)
	require.Contains(t, reply, "restart original service")
	require.Contains(t, reply, "后续汇总未完成")
	require.Contains(t, reply, "人工")
	require.Equal(t, 1, runner.calls)
	require.True(t, eng.instanceOpsInterruptionIncludedInReplyThisTurn,
		"an already-present canonical report still needs a transport acknowledgement")
	eng.AcknowledgeDeliveredInstanceOpsInterruption()
	require.Nil(t, eng.pendingInstanceOpsInterruption)
}

func TestInstanceOpsDifferentTargetsUseDifferentInvocations(t *testing.T) {
	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "已核实"}}
	eng := newInstanceOpsEngine(runner, nil)
	for _, id := range []string{"uhost-1", "cpod-2", "uhost-1"} {
		callID := []string{"first", "second", "third"}[runner.calls]
		requireInstanceOpsObservation(t, eng.executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", callID, map[string]any{"UHostId": id, "Task": "检查服务"}, noopStep))
		require.Equal(t, id, runner.lastReq.InstanceID)
	}
	require.Equal(t, 3, runner.calls)
}
