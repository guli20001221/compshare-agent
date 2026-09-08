package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

func TestBudgetClosingPreservesTaskHistoryAndToolResults(t *testing.T) {
	const previous = "后续都请用英文回答。"
	const question = "解释 vLLM 显存不足的处理，并核对 uhost-a 当前运行状态。"
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("search", "SearchKnowledge", `{"query":"vllm 显存不足"}`)}, Usage: llm.TokenUsage{TotalTokens: 60000}},
		{Content: "Reduce max-model-len. The instance status has not been checked.[[ext-vllm-oom-001]]"},
	}}
	eng := NewWithDeps(model, &mockExecutor{}, nil)
	eng.InitWithContext("test account")
	eng.messages = append(eng.messages,
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: previous},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "Understood."},
	)
	eng.maxTokensPerTurn = 50000
	eng.SetKnowledgeRetriever(vllmRetriever())
	reply, err := eng.Chat(context.Background(), question, noopStep)
	require.NoError(t, err)
	require.Len(t, model.calls, 2)
	closing := model.calls[1]
	text := renderTestMessages(closing.Messages)
	require.Contains(t, text, previous)
	require.Contains(t, text, question)
	require.Contains(t, text, "你是本轮唯一的业务判断者")
	require.Contains(t, text, "max-model-len")
	require.Nil(t, closing.ResponseFormat)
	require.Empty(t, closing.Tools)
	require.Contains(t, reply, "has not been checked")
	require.NotContains(t, reply, "[[ext-vllm-oom-001]]")
}

func TestBudgetClosingUsesPlainReadResultsWithoutRAG(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("read", "ReadCapability_resource_info", `{}`)}, Usage: llm.TokenUsage{TotalTokens: 60000}},
		{Content: "当前没有实例。"},
	}}
	eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{}, "TotalCount": 0},
	}}, nil)
	eng.maxTokensPerTurn = 50000
	reply, err := eng.Chat(context.Background(), "看看我的实例", noopStep)
	require.NoError(t, err)
	require.Len(t, model.calls, 2)
	require.Contains(t, renderTestMessages(model.calls[1].Messages), "看看我的实例")
	require.Equal(t, "当前没有实例。", reply)
}

func TestClosingRequiresCompletedWorkAndCompleteModelOutput(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{{Content: "unfinished", StopReason: "length", Usage: llm.TokenUsage{TotalTokens: 12}}}}
	eng := NewWithDeps(model, &mockExecutor{}, nil)
	_, ok := eng.finishAgentTurn(context.Background())
	require.False(t, ok)
	require.Empty(t, model.calls)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "看看我的实例"},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "read", Content: "tool result"},
	}
	_, ok = eng.finishAgentTurn(context.Background())
	require.False(t, ok)
	require.Len(t, model.calls, 1)
	require.Equal(t, 12, eng.turnTokensConsumed)
}
