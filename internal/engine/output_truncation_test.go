package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/agentruntime"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

func TestLengthStoppedResponseIsRetriedWithoutCommittingItsPartialText(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{
		{Content: "这段不完整的回答绝不能进入历史", StopReason: "length"},
		{Content: "这是完整且可提交的回答。"},
	}}
	eng := NewWithDeps(model, &mockExecutor{}, nil)

	reply, err := eng.Chat(context.Background(), "解释一下", noopStep)
	require.NoError(t, err)
	require.Equal(t, "这是完整且可提交的回答。", reply)
	require.Len(t, model.calls, 2, "the first length stop must receive one bounded recovery attempt")

	var sawRecoveryInstruction bool
	for _, message := range model.calls[1].Messages {
		if message.Role == openai.ChatMessageRoleSystem && message.Content == truncatedOutputRecoveryInstruction {
			sawRecoveryInstruction = true
		}
	}
	require.True(t, sawRecoveryInstruction, "the retry must know the prior output was discarded")

	for _, message := range eng.messages {
		require.NotContains(t, message.Content, "这段不完整的回答绝不能进入历史")
	}
}

func TestLengthStoppedToolCallIsNeverExecuted(t *testing.T) {
	executor := &mockExecutor{}
	model := &mockLLM{responses: []llm.ChatResponse{
		{
			StopReason: "length",
			ToolCalls:  []openai.ToolCall{toolCall("partial-tool", "ReadCapability_resource_info", `{}`)},
		},
		{Content: "我需要先重新确认后再继续。"},
	}}
	eng := NewWithDeps(model, executor, nil)

	_, err := eng.Chat(context.Background(), "查看实例", noopStep)
	require.NoError(t, err)
	require.Empty(t, executor.calls, "a tool call carried by a length-stopped response is not authoritative")
	require.Len(t, model.calls, 2)
	for _, message := range model.calls[1].Messages {
		require.Empty(t, message.ToolCalls, "the discarded tool call must not enter the next request either")
		require.NotEqual(t, "partial-tool", message.ToolCallID)
	}
}

func TestRepeatedLengthStopsEndWithAnHonestRefusal(t *testing.T) {
	const sensitiveReply = "Jupyter Token：server-owned-token"
	model := &mockLLM{responses: []llm.ChatResponse{
		{Content: "第一次截断", StopReason: "length"},
		{Content: "第二次截断", StopReason: "max_tokens"},
	}}
	eng := NewWithDeps(model, &mockExecutor{}, nil)
	limiter := &scriptedRateLimiter{}
	limiter.before = func(governance.Request) {
		eng.sensitiveRepliesThisTurn = []string{sensitiveReply}
	}
	eng.rateLimiter = limiter
	var completions []observability.TurnCompletionTrace
	eng.SetTurnCompletionObserver(func(trace observability.TurnCompletionTrace) {
		completions = append(completions, trace)
	})

	var delivered []string
	reply, err := eng.ChatWithOptions(context.Background(), "解释一下", noopStep, ChatOptions{
		OnTextDelta: func(delta string) { delivered = append(delivered, delta) },
	})
	require.NoError(t, err)
	want := sensitiveReply + "\n\n" + outputTruncatedRefusal
	require.Equal(t, want, reply)
	require.Equal(t, want, strings.Join(delivered, ""))
	require.Len(t, model.calls, 2)
	require.Len(t, completions, 1)
	require.Equal(t, observability.CompletionReasonModelOutputTruncated, completions[0].Reason)

	events := eng.AgentRuntimeEventsThisTurn()
	require.NotEmpty(t, events)
	require.Equal(t, agentruntime.FinishOutputTruncated, events[len(events)-1].FinishReason)
	for _, message := range eng.messages {
		require.NotContains(t, message.Content, "第一次截断")
		require.NotContains(t, message.Content, "第二次截断")
	}
}
