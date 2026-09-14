package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

func TestLiveReadRefreshesAfterCommittedWorkflowAndKnowledgeRemainsReusable(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{
			toolCall("before", "ReadCapability_resource_info", `{}`),
			toolCall("knowledge", "SearchKnowledge", `{"query":"开机规则"}`),
		}},
		{ToolCalls: []openai.ToolCall{toolCall("start", "RequestStartInstance", `{"UHostId":"uhost-a","StartMode":"normal"}`)}},
		{ToolCalls: []openai.ToolCall{
			toolCall("after", "ReadCapability_resource_info", `{}`),
			toolCall("knowledge-again", "SearchKnowledge", `{"query":"开机规则"}`),
		}},
		{Content: "实例已运行。"},
	}}
	state := "Stopped"
	describesAfterWrite, writes := 0, 0
	executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			if writes > 0 {
				describesAfterWrite++
			}
			return map[string]any{"RetCode": 0, "TotalCount": float64(1), "UHostSet": []any{map[string]any{
				"UHostId": "uhost-a", "Name": "test-a", "State": state,
				"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "ChargeType": "Postpay",
			}}}, nil
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb"}}}, nil
		case "StartCompShareInstance":
			writes++
			state = "Running"
		}
		return map[string]any{"RetCode": 0}, nil
	}}
	retriever := &scriptedKnowledgeRetriever{}
	eng := NewWithDeps(model, executor, alwaysConfirm)
	eng.SetMutatingToolsEnabled(true)
	eng.SetKnowledgeRetriever(retriever)
	_, err := eng.Chat(context.Background(), "先查看 uhost-a 和开机规则，再开机，然后重新查看状态", noopStep)
	require.NoError(t, err)
	require.Equal(t, 1, writes)
	require.Len(t, model.calls, 4)
	require.Contains(t, mutationTestObservation(t, model.calls[1], "before"), `"value":"Stopped"`)
	after := mutationTestObservation(t, model.calls[3], "after")
	require.Equal(t, 1, describesAfterWrite, "same-argument verification must reach the upstream after a committed write")
	require.Contains(t, after, `"value":"Running"`)
	require.NotContains(t, after, "reused_observation")
	require.Len(t, retriever.calls, 1, "a platform write does not invalidate knowledge evidence")
	require.Contains(t, mutationTestObservation(t, model.calls[3], "knowledge-again"), "reused_observation")
}

func TestLiveMonitorRefreshesAfterInstanceOpsReturns(t *testing.T) {
	for _, scenario := range []string{"completed", "partial_report", "transport_interrupted"} {
		t.Run(scenario, func(t *testing.T) {
			runner := &fakeInstanceOpsRunner{
				progress: []InstanceOpsProgress{{Kind: InstanceOpsProgressCommand, Tier: "mutating", Disposition: "ran", Command: "systemctl restart app"}},
				verdict:  InstanceOpsVerdict{Text: "已重启服务，请复核 CPU 使用率。", Ran: 1},
			}
			if scenario == "partial_report" {
				runner.verdict.AgentFailed = true
				runner.verdict.ErrClass = "model_transport"
			}
			if scenario == "transport_interrupted" {
				runner.err = errors.New("runner disconnected after restarting the service")
			}
			const monitorArgs = `{"targets":[{"type":"uhost_id_user_input","value":"uhost-a"}],"metrics":["cpu"]}`
			model := &mockLLM{responses: []llm.ChatResponse{
				{ToolCalls: []openai.ToolCall{toolCall("before", "ReadCapability_monitor_query", monitorArgs)}},
				{ToolCalls: []openai.ToolCall{toolCall("repair", "DiagnoseInstanceInternals", `{"UHostId":"uhost-a","Task":"修复 CPU 占用异常"}`)}},
				{ToolCalls: []openai.ToolCall{toolCall("after", "ReadCapability_monitor_query", monitorArgs)}},
				{Content: "已复核监控。"},
			}}
			monitorCalls := 0
			executor := &mockExecutorFn{fn: func(action string, _ map[string]any) (map[string]any, error) {
				switch action {
				case "DescribeCompShareInstance":
					return map[string]any{"TotalCount": float64(1), "UHostSet": []any{map[string]any{
						"UHostId": "uhost-a", "Name": "test-a", "State": "Running", "Zone": "cn-wlcb-01",
					}}}, nil
				case "GetCompShareInstanceMonitor":
					monitorCalls++
					cpu := float64(90)
					if runner.calls > 0 {
						cpu = 12.5
					}
					return map[string]any{"Data": map[string]any{"List": []any{map[string]any{
						"UHostId": "uhost-a", "Metrics": []any{map[string]any{
							"MetricKey": "uhost_cpu_used", "Results": []any{map[string]any{
								"Values": []any{map[string]any{"Value": cpu, "Timestamp": float64(1778420000)}},
							}},
						}},
					}}}}, nil
				}
				return map[string]any{"RetCode": 0}, nil
			}}
			eng := NewWithDeps(model, executor, nil)
			eng.SetMutatingToolsEnabled(true)
			eng.SetInstanceOps(runner)
			_, err := eng.Chat(context.Background(), "查看 uhost-a 的 CPU，修复服务后再查一次", noopStep)
			require.NoError(t, err)
			require.Equal(t, 1, runner.calls)
			require.Len(t, model.calls, 4)
			require.Equal(t, 2, monitorCalls, "an interrupted Guest run may already have changed the monitored state")
			after := mutationTestObservation(t, model.calls[3], "after")
			require.Contains(t, after, "12.5")
			require.NotContains(t, after, "reused_observation")
		})
	}
}

func mutationTestObservation(t *testing.T, request llm.ChatRequest, callID string) string {
	t.Helper()
	for _, message := range request.Messages {
		if message.Role == openai.ChatMessageRoleTool && message.ToolCallID == callID {
			return message.Content
		}
	}
	t.Fatalf("missing observation for %s", callID)
	return ""
}
