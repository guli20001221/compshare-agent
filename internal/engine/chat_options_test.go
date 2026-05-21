package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deltaMockLLM returns a final text reply and fires OnTextDelta for each
// Chinese character in the response, simulating a streaming LLM.
type deltaMockLLM struct {
	reqs []llm.ChatRequest
}

func (m *deltaMockLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	m.reqs = append(m.reqs, req)
	if req.OnTextDelta != nil {
		req.OnTextDelta("你")
		req.OnTextDelta("好")
	}
	return &llm.ChatResponse{
		Content: "你好",
		Usage:   llm.TokenUsage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
	}, nil
}

func TestChatWithOptionsStreamsTextDeltas(t *testing.T) {
	client := &deltaMockLLM{}
	eng := NewWithDeps(client, &mockExecutor{}, func(string, map[string]any) bool { return false })
	eng.InitWithContext("用户当前没有实例。")

	var deltas []string
	var usage llm.TokenUsage
	reply, err := eng.ChatWithOptions(context.Background(), "你好", nil, ChatOptions{
		OnTextDelta: func(s string) { deltas = append(deltas, s) },
		OnUsage:     func(u llm.TokenUsage) { usage = u },
	})

	require.NoError(t, err)
	assert.Equal(t, "你好", reply)
	assert.Equal(t, []string{"你", "好"}, deltas)
	assert.Equal(t, 3, usage.TotalTokens)
	require.Len(t, client.reqs, 1)
	assert.NotNil(t, client.reqs[0].OnTextDelta)
}

func TestChatWithOptionsUsageCalledOnFinalTextBranch(t *testing.T) {
	client := &deltaMockLLM{}
	eng := NewWithDeps(client, &mockExecutor{}, func(string, map[string]any) bool { return false })
	eng.InitWithContext("用户当前没有实例。")

	usageCalled := false
	_, err := eng.ChatWithOptions(context.Background(), "你好", nil, ChatOptions{
		OnUsage: func(u llm.TokenUsage) { usageCalled = true },
	})
	require.NoError(t, err)
	assert.True(t, usageCalled)
}

func TestChatDelegatesToChatWithOptions(t *testing.T) {
	client := &deltaMockLLM{}
	eng := NewWithDeps(client, &mockExecutor{}, func(string, map[string]any) bool { return false })
	eng.InitWithContext("用户当前没有实例。")

	reply, err := eng.Chat(context.Background(), "你好", nil)
	require.NoError(t, err)
	assert.Equal(t, "你好", reply)
}

func TestRehydrateHistoryBuildsSystemUserAssistantMessages(t *testing.T) {
	eng := NewWithDeps(&deltaMockLLM{}, &mockExecutor{}, func(string, map[string]any) bool { return false })

	eng.RehydrateHistory([]HistoryMessage{
		{Role: openai.ChatMessageRoleUser, Content: "第一问"},
		{Role: openai.ChatMessageRoleAssistant, Content: "第一答"},
	})

	require.Len(t, eng.messages, 3)
	assert.Equal(t, openai.ChatMessageRoleSystem, eng.messages[0].Role)
	assert.Equal(t, openai.ChatMessageRoleUser, eng.messages[1].Role)
	assert.Equal(t, "第一问", eng.messages[1].Content)
	assert.Equal(t, openai.ChatMessageRoleAssistant, eng.messages[2].Role)
	assert.Equal(t, "第一答", eng.messages[2].Content)
}

func TestRehydrateHistorySkipsEmptyContent(t *testing.T) {
	eng := NewWithDeps(&deltaMockLLM{}, &mockExecutor{}, func(string, map[string]any) bool { return false })

	eng.RehydrateHistory([]HistoryMessage{
		{Role: openai.ChatMessageRoleUser, Content: ""},
		{Role: openai.ChatMessageRoleUser, Content: "valid"},
	})

	// system + one valid user message
	require.Len(t, eng.messages, 2)
	assert.Equal(t, openai.ChatMessageRoleUser, eng.messages[1].Role)
	assert.Equal(t, "valid", eng.messages[1].Content)
}

func TestRehydrateHistorySkipsNonUserNonAssistantRoles(t *testing.T) {
	eng := NewWithDeps(&deltaMockLLM{}, &mockExecutor{}, func(string, map[string]any) bool { return false })

	eng.RehydrateHistory([]HistoryMessage{
		{Role: openai.ChatMessageRoleTool, Content: "tool result"},
		{Role: openai.ChatMessageRoleSystem, Content: "another system"},
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
	})

	// system + one valid user message
	require.Len(t, eng.messages, 2)
	assert.Equal(t, openai.ChatMessageRoleSystem, eng.messages[0].Role)
	assert.Equal(t, "hi", eng.messages[1].Content)
}
