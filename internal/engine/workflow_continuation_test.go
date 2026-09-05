package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

func TestCommittedWorkflowContinuesTheUsersRemainingTask(t *testing.T) {
	for _, scenario := range []string{"rename_same_batch", "invoice_same_batch", "invoice_next_round", "decline_second_write"} {
		t.Run(scenario, func(t *testing.T) {
			stop := toolCall("stop", "RequestStopInstance", `{"UHostId":"uhost-a"}`)
			next := toolCall("invoice", "ReadCapability_invoice_status", `{}`)
			question := "关闭 uhost-a，然后查询我的发票状态"
			secondWrite := scenario == "rename_same_batch" || scenario == "decline_second_write"
			if secondWrite {
				next = toolCall("rename", "RequestRenameInstance", `{"UHostId":"uhost-a","Name":"demo-stop"}`)
				question = "关闭 uhost-a，然后把它改名为 demo-stop"
			}
			responses := []llm.ChatResponse{{ToolCalls: []openai.ToolCall{stop, next}}}
			if scenario == "invoice_next_round" {
				responses = []llm.ChatResponse{{ToolCalls: []openai.ToolCall{stop}}, {ToolCalls: []openai.ToolCall{next}}}
			}
			responses = append(responses, llm.ChatResponse{Content: "剩余任务也已处理。"})
			model := &mockLLM{responses: responses}
			state, name := "Running", "train-a"
			executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
				switch action {
				case "DescribeCompShareInstance":
					return map[string]any{"UHostSet": []any{map[string]any{"UHostId": "uhost-a", "Name": name, "State": state, "Zone": "cn-wlcb-01", "ChargeType": "Postpay"}}}, nil
				case "DescribeCompShareSupportZone":
					return map[string]any{"ZoneInfo": []any{map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb"}}}, nil
				case "StopCompShareInstance":
					state = "Stopping"
				case "ModifyCompShareInstanceName":
					name = args["Name"].(string)
				case "GetCompShareInvoiceIssued":
					return map[string]any{"InvoiceSet": []any{}, "TotalCount": 0}, nil
				}
				return map[string]any{"RetCode": 0}, nil
			}}
			var cards []string
			eng := NewWithDeps(model, executor, func(action string, _ map[string]any) bool {
				cards = append(cards, action)
				return scenario != "decline_second_write" || len(cards) == 1
			})
			eng.SetMutatingToolsEnabled(true)
			reply, err := eng.Chat(context.Background(), question, noopStep)
			require.NoError(t, err)
			require.Equal(t, "Stopping", state)
			if scenario == "decline_second_write" {
				require.Equal(t, "train-a", name)
				require.NotContains(t, executor.calls, "ModifyCompShareInstanceName")
				require.Contains(t, reply, "提交关机请求")
				require.Contains(t, reply, "重命名操作未执行")
				require.Len(t, eng.committedWriteRepliesThisTurn, 1)
			} else {
				require.Equal(t, "剩余任务也已处理。", reply)
				require.GreaterOrEqual(t, len(model.calls), 2)
				if secondWrite {
					require.Equal(t, "demo-stop", name)
					require.Len(t, eng.committedWriteRepliesThisTurn, 2)
				} else {
					require.Contains(t, executor.calls, "GetCompShareInvoiceIssued")
				}
			}
			if secondWrite {
				require.Equal(t, []string{"StopInstanceWorkflow", "RenameInstanceWorkflow"}, cards)
			} else {
				require.Equal(t, []string{"StopInstanceWorkflow"}, cards)
			}
			for _, message := range eng.messages {
				if message.ToolCallID == next.ID {
					require.NotEqual(t, "skipped", message.Content)
				}
			}
		})
	}
}

func TestCommittedWorkflowSurvivesCancellationBeforeTheNextRound(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := &mockLLM{responses: []llm.ChatResponse{{ToolCalls: []openai.ToolCall{
		toolCall("stop", "RequestStopInstance", `{"UHostId":"uhost-a"}`),
	}}}}
	executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
		switch action {
		case "DescribeCompShareInstance":
			return map[string]any{"UHostSet": []any{map[string]any{
				"UHostId": "uhost-a", "Name": "test-a", "State": "Running", "Zone": "cn-wlcb-01", "ChargeType": "Postpay",
			}}}, nil
		case "DescribeCompShareSupportZone":
			return map[string]any{"ZoneInfo": []any{map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb"}}}, nil
		}
		return map[string]any{"RetCode": 0}, nil
	}}
	eng := NewWithDeps(model, executor, func(string, map[string]any) bool { return true })
	eng.SetMutatingToolsEnabled(true)
	reply, err := eng.Chat(ctx, "关机 uhost-a，然后查询发票进度", func(ev StepEvent) {
		if ev.Type == StepToolResult && ev.Action == "StopCompShareInstance" {
			cancel()
		}
	})
	require.NoError(t, err)
	require.Len(t, eng.committedWriteRepliesThisTurn, 1)
	require.Contains(t, reply, "提交关机请求")
	require.Contains(t, reply, "请勿重复提交")
	require.Len(t, model.calls, 1)
	require.Equal(t, reply, eng.messages[len(eng.messages)-1].Content)
}
