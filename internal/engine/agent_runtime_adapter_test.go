package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/agentruntime"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

func TestChatRunsThroughAgentRuntimeLifecycle(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("tc1", "ReadCapability_resource_info", `{}`)}},
		{Content: "4090 有 24GB 显存"},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}

	reply, err := eng.Chat(context.Background(), "4090 什么配置", noopStep)
	require.NoError(t, err)
	require.Equal(t, "4090 有 24GB 显存", reply)

	events := eng.AgentRuntimeEventsThisTurn()
	require.Equal(t, []agentruntime.Event{
		{Type: agentruntime.EventRoundStarted, Round: 0},
		{Type: agentruntime.EventModelStep, Round: 0, ToolCallCount: 1},
		{Type: agentruntime.EventObservation, Round: 0, Action: "ReadCapability_resource_info"},
		{Type: agentruntime.EventRoundStarted, Round: 1},
		{Type: agentruntime.EventModelStep, Round: 1, HasFinalText: true},
		{Type: agentruntime.EventFinished, Round: 1, FinishReason: agentruntime.FinishFinalAnswer},
	}, events)
}

func TestChatRoundCeilingIsReportedByAgentRuntime(t *testing.T) {
	responses := make([]llm.ChatResponse, maxReActRounds)
	for i := range responses {
		responses[i] = llm.ChatResponse{ToolCalls: []openai.ToolCall{
			toolCall("tc", "ReadCapability_resource_info", `{}`),
		}}
	}
	eng := NewWithDeps(&mockLLM{responses: responses}, &mockExecutor{}, nil)
	eng.messages = []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "test"}}

	_, err := eng.Chat(context.Background(), "test", noopStep)
	require.NoError(t, err)
	events := eng.AgentRuntimeEventsThisTurn()
	require.NotEmpty(t, events)
	require.Equal(t, agentruntime.EventFinished, events[len(events)-1].Type)
	require.Equal(t, agentruntime.FinishRoundLimit, events[len(events)-1].FinishReason)
}
