package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/agentprotocol"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/refusal"
	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

func customerSupportToolCall() llm.ChatResponse {
	return llm.ChatResponse{ToolCalls: []openai.ToolCall{
		toolCall("support", tools.CustomerSupportHandoffName, `{}`),
	}}
}

func TestChatCustomerSupportHandoffIsAnAgentDecision(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{customerSupportToolCall()}}
	eng := NewWithDeps(model, &mockExecutor{}, nil)
	onStep, steps := collectSteps()

	reply, err := eng.Chat(context.Background(), "请帮我转接人工客服", onStep)
	require.NoError(t, err)
	require.Equal(t, refusal.HumanAgentTransfer, reply)
	require.Len(t, model.calls, 1, "the request must reach the central Agent")
	require.Contains(t, toolNames(model.calls[0].Tools), tools.CustomerSupportHandoffName)
	require.Len(t, *steps, 2)
	require.Equal(t, StepToolCall, (*steps)[0].Type)
	require.Equal(t, StepToolResult, (*steps)[1].Type)
	require.Equal(t, tools.CustomerSupportHandoffName, (*steps)[0].Action)
	require.Contains(t, (*steps)[1].Message, "未确认接通或受理")

	toolJSON, err := json.Marshal(model.calls[0].Tools)
	require.NoError(t, err)
	require.NotContains(t, string(toolJSON), "qrcode.png", "the model must not author the channel renderer")
	require.NotContains(t, string(toolJSON), agentprotocol.FeishuCustomerSupportMarker)
}

func TestChatCustomerSupportMentionsRemainWithTheCentralAgent(t *testing.T) {
	cases := []string{
		"不要转人工，继续帮我排查",
		"你刚才说的转人工是什么意思？",
		"人工客服能解决什么问题？",
		"我先不找人工客服",
	}
	for _, question := range cases {
		t.Run(question, func(t *testing.T) {
			const answer = "好的，我继续根据你的问题处理。"
			model := &mockLLM{responses: []llm.ChatResponse{{Content: answer}}}
			eng := NewWithDeps(model, &mockExecutor{}, nil)

			reply, err := eng.Chat(context.Background(), question, noopStep)
			require.NoError(t, err)
			require.Equal(t, answer, reply)
			require.Len(t, model.calls, 1, "a mention must not be intercepted before semantic interpretation")
			require.NotContains(t, reply, "qrcode.png")
		})
	}
}

func TestFeishuModesUseTheirSupportRenderer(t *testing.T) {
	for name, opts := range map[string]ChatOptions{
		"public platform without console handoff":    {PublicPlatformReadOnly: true},
		"knowledge only with console handoff":        {KnowledgeOnly: true, FeishuConsoleHandoff: true},
		"knowledge precedence keeps Feishu renderer": {KnowledgeOnly: true, PublicPlatformReadOnly: true},
	} {
		t.Run(name, func(t *testing.T) {
			model := &mockLLM{responses: []llm.ChatResponse{customerSupportToolCall()}}
			eng := NewWithDeps(model, &mockExecutor{}, nil)

			reply, err := eng.ChatWithOptions(context.Background(), "我需要人工客服", noopStep, opts)
			require.NoError(t, err)
			require.Equal(t, agentprotocol.FeishuCustomerSupportMarker, reply)
			require.Len(t, model.calls, 1)
			require.Contains(t, toolNames(model.calls[0].Tools), tools.CustomerSupportHandoffName)
			require.NotContains(t, model.calls[0].Messages[0].Content, agentprotocol.FeishuCustomerSupportMarker,
				"the adapter marker is emitted only by the tool executor")
			require.NotContains(t, strings.Join(messageContents(model.calls[0].Messages), "\n"), "qrcode.png")
		})
	}
}

func TestCustomerSupportDisplayProjectionDoesNotEnterColdModelHistory(t *testing.T) {
	hotModel := &mockLLM{responses: []llm.ChatResponse{customerSupportToolCall()}}
	hot := NewWithDeps(hotModel, &mockExecutor{}, nil)
	reply, err := hot.Chat(context.Background(), "请帮我转接人工客服", noopStep)
	require.NoError(t, err)
	require.Equal(t, refusal.HumanAgentTransfer, reply)
	transcript, stats := hot.LastTurnTranscript()
	require.True(t, stats.Attempted)
	require.NotEmpty(t, transcript)

	coldModel := &mockLLM{responses: []llm.ChatResponse{{Content: "我继续处理。"}}}
	cold := NewWithDeps(coldModel, &mockExecutor{}, nil)
	cold.RehydrateHistory([]HistoryMessage{
		{Role: openai.ChatMessageRoleUser, Content: "请帮我转接人工客服"},
		{Role: openai.ChatMessageRoleAssistant, Content: refusal.HumanAgentTransfer, Transcript: transcript},
	})
	_, err = cold.Chat(context.Background(), "继续", noopStep)
	require.NoError(t, err)
	require.Len(t, coldModel.calls, 1)
	history := strings.Join(messageContents(coldModel.calls[0].Messages), "\n")
	require.Contains(t, history, agentprotocol.CustomerSupportHistoryCompletion)
	require.Contains(t, history, "本次未返回备用入口或工单地址")
	require.NotContains(t, history, "qrcode.png")
	require.NotContains(t, history, agentprotocol.FeishuCustomerSupportMarker)
}

func TestUnavailableSupportEntryReportStaysWithTheCentralAgent(t *testing.T) {
	const answer = "你反馈这个客服入口已满，我没有已核实的替代入口，暂时无法帮你接通人工。"
	model := &mockLLM{responses: []llm.ChatResponse{customerSupportToolCall(), {Content: answer}}}
	eng := NewWithDeps(model, &mockExecutor{}, nil)
	_, err := eng.Chat(context.Background(), "请帮我联系人工客服", noopStep)
	require.NoError(t, err)
	reply, err := eng.Chat(context.Background(), "上面的客服入口显示群人数已满", noopStep)
	require.NoError(t, err)
	require.Equal(t, answer, reply)
	require.NotContains(t, reply, "qrcode.png")
	require.Len(t, model.calls, 2, "entry availability is interpreted by the central Agent, not a keyword router")
	history := strings.Join(messageContents(model.calls[1].Messages), "\n")
	require.Contains(t, history, "群人数已满")
	require.Contains(t, history, agentprotocol.CustomerSupportHistoryCompletion)
	require.NotContains(t, history, "qrcode.png")
	toolJSON, err := json.Marshal(model.calls[1].Tools)
	require.NoError(t, err)
	require.Contains(t, string(toolJSON), "满员或失效时不要重复调用")
	require.Contains(t, string(toolJSON), "不编造工单菜单或接待状态")
}

func messageContents(messages []openai.ChatCompletionMessage) []string {
	out := make([]string, 0, len(messages))
	for _, message := range messages {
		out = append(out, message.Content)
	}
	return out
}

func TestHumanAgentTransferReplyContainsSupportQR(t *testing.T) {
	require.Contains(t, refusal.HumanAgentTransfer, "ucompshare-picture.cn-wlcb.ufileos.com/QRCode/qrcode.png")
	require.True(t, strings.HasPrefix(refusal.HumanAgentTransfer, "如需人工客服协助，请扫描"))
	require.NotContains(t, refusal.HumanAgentTransfer, "已为您转接")
	require.NotContains(t, refusal.HumanAgentTransfer, "会有专人为您服务")
	require.Contains(t, refusal.HumanAgentTransfer, "不代表已接通或受理")
}
