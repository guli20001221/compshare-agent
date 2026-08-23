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

func TestCustomerSupportHandoffUsesTheExistingFeishuRendererProtocol(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{customerSupportToolCall()}}
	eng := NewWithDeps(model, &mockExecutor{}, nil)

	reply, err := eng.ChatWithOptions(context.Background(), "我需要人工客服", noopStep, ChatOptions{
		KnowledgeOnly:        true,
		FeishuConsoleHandoff: true,
	})
	require.NoError(t, err)
	require.Equal(t, agentprotocol.FeishuCustomerSupportMarker, reply)
	require.Len(t, model.calls, 1)
	require.Contains(t, toolNames(model.calls[0].Tools), tools.CustomerSupportHandoffName)
	require.NotContains(t, model.calls[0].Messages[0].Content, agentprotocol.FeishuCustomerSupportMarker,
		"the adapter marker is emitted only by the tool executor")
	require.NotContains(t, strings.Join(messageContents(model.calls[0].Messages), "\n"), "qrcode.png")
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
	require.True(t, strings.HasPrefix(refusal.HumanAgentTransfer, "好的，已为您转接人工客服"))
}
