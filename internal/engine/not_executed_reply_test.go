package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/workflow"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

// Every card that does not end in approval reaches the reply as the same
// refusal. The sentence must say what actually happened to the card: the user
// who ran out of time did not cancel, and one whose connection dropped did
// nothing at all.
func TestNotExecutedReplyNamesTheCardsTerminalReason(t *testing.T) {
	for reason, want := range map[string]string{
		observability.ConfirmationReasonUserDeclined:     "好的，创建实例操作未执行。",
		observability.ConfirmationReasonTimeout:          "确认卡超时未收到回应，创建实例操作未执行。",
		observability.ConfirmationReasonClientDisconnect: "连接已中断，创建实例操作未执行。",
		observability.ConfirmationReasonDeliveryFailed:   "确认卡未能送达，创建实例操作未执行。",
		observability.ConfirmationReasonBrokerCancelled:  "确认卡未能送达，创建实例操作未执行。",
		"": "好的，创建实例操作未执行。",
	} {
		t.Run("reason="+reason, func(t *testing.T) {
			got := notExecutedReply("CreateInstanceWorkflow", reason)
			require.Contains(t, got, want)
			require.Contains(t, got, "如需继续，请重新发送指令并确认。")
			if reason != observability.ConfirmationReasonUserDeclined && reason != "" {
				require.NotContains(t, got, "好的", "only a decline is acknowledged as the user's choice")
			}
		})
	}
}

// The reason must reach the sentence through the real turn: a committed stop
// followed by a rename card that timed out. The reply names the timeout, and the
// committed write survives as the summary the gateway persists on a dropped
// transport.
func TestTimedOutCardIsNotPhrasedAsADeclineAndTheCommittedWriteStaysReportable(t *testing.T) {
	for _, tc := range []struct {
		reason string
		want   string
	}{
		{reason: observability.ConfirmationReasonTimeout, want: "确认卡超时未收到回应，重命名操作未执行"},
		{reason: observability.ConfirmationReasonClientDisconnect, want: "连接已中断，重命名操作未执行"},
		{reason: observability.ConfirmationReasonUserDeclined, want: "好的，重命名操作未执行"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			model := &mockLLM{responses: []llm.ChatResponse{{ToolCalls: []openai.ToolCall{
				toolCall("stop", "RequestStopInstance", `{"UHostId":"uhost-a"}`),
				toolCall("rename", "RequestRenameInstance", `{"UHostId":"uhost-a","Name":"demo-stop"}`),
			}}}}
			state := "Running"
			executor := &mockExecutorFn{fn: func(action string, args map[string]any) (map[string]any, error) {
				switch action {
				case "DescribeCompShareInstance":
					return map[string]any{"UHostSet": []any{map[string]any{"UHostId": "uhost-a", "Name": "train-a", "State": state, "Zone": "cn-wlcb-01", "ChargeType": "Postpay"}}}, nil
				case "DescribeCompShareSupportZone":
					return map[string]any{"ZoneInfo": []any{map[string]any{"Zone": "cn-wlcb-01", "Region": "cn-wlcb"}}}, nil
				case "StopCompShareInstance":
					state = "Stopping"
				}
				return map[string]any{"RetCode": 0}, nil
			}}
			eng := NewWithDeps(model, executor, nil)
			eng.SetMutatingToolsEnabled(true)
			cards := 0

			reply, err := eng.ChatWithOptions(context.Background(), "关闭 uhost-a，然后把它改名为 demo-stop", noopStep, ChatOptions{
				ConfirmResultFunc: func(string, map[string]any) ConfirmationResult {
					cards++
					if cards == 1 {
						return ConfirmationResult{Confirmed: true}
					}
					return ConfirmationResult{TerminalReason: tc.reason}
				},
			})

			require.NoError(t, err)
			require.Equal(t, 2, cards)
			require.Equal(t, "Stopping", state)
			require.NotContains(t, executor.calls, "ModifyCompShareInstanceName")
			require.Contains(t, reply, "提交关机请求", "the committed write leads the reply")
			require.Contains(t, reply, tc.want)
			summary := eng.CommittedWriteSummary()
			require.Contains(t, summary, "uhost-a")
			require.Contains(t, summary, "提交关机请求")
			require.NotContains(t, summary, "未执行", "the summary carries only what committed")
		})
	}
}

func TestCommittedWriteSummaryIsEmptyWhenNothingCommitted(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	require.Equal(t, "", eng.CommittedWriteSummary())
	var nilEngine *Engine
	require.Equal(t, "", nilEngine.CommittedWriteSummary())
}

// Guided creation confirms through the form gate. The refusal sentence reads
// the same terminal-reason field the trace does, so the form gate must write
// it — otherwise a guided card that timed out at 2 a.m. reads as 「好的」, as if
// the user had declined.
func TestFormGateRecordsTheTerminalReasonForTheRefusalSentence(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	restore := eng.installTurnConfirmation(ChatOptions{
		ConfirmEditsFunc: func(string, map[string]any, *workflow.ConfirmForm) workflow.ConfirmResolution {
			return workflow.ConfirmResolution{TerminalReason: observability.ConfirmationReasonTimeout}
		},
	})
	defer restore()

	resolution := eng.confirmEditsFn("CreateInstanceWorkflow", nil, &workflow.ConfirmForm{})

	require.False(t, resolution.Confirmed)
	require.Equal(t, observability.ConfirmationReasonTimeout, eng.lastConfirmationTerminalReason)
	require.Contains(t, notExecutedReply("CreateInstanceWorkflow", eng.lastConfirmationTerminalReason), "确认卡超时未收到回应")
}
