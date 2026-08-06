package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

func TestAgentAuthorsOrdinaryReadResultWithoutServerInjection(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("read", capability.ReadToolName(intent.IntentResourceInfo), `{}`)}},
		{Content: "你有一台正在运行的 4090 实例 train-007（uhost-exact-123）。"},
	}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"TotalCount": float64(1), "UHostSet": []any{map[string]any{
			"UHostId": "uhost-exact-123", "Name": "train-007", "State": "Running", "GpuType": "4090", "GPU": float64(1), "CPU": float64(16), "Memory": float64(64),
		}}},
	}}
	eng := NewWithDeps(model, executor, nil)

	reply, err := eng.Chat(context.Background(), "我有哪些实例", noopStep)
	require.NoError(t, err)
	require.Contains(t, reply, "正在运行")
	require.Contains(t, reply, "uhost-exact-123")
	require.Contains(t, reply, "train-007")
	require.Len(t, eng.platformReadEvidenceThisTurn, 1,
		"ordinary reads keep proof for server-side checks without adding a server-rendered response block")
	require.Empty(t, eng.sensitiveRepliesThisTurn)
}

func TestNaturalAssistantMessageEndsTurnWithoutFinishTool(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{{Content: "这是最终回答。"}}}
	eng := NewWithDeps(model, &mockExecutor{}, nil)

	reply, err := eng.Chat(context.Background(), "解释一下", noopStep)
	require.NoError(t, err)
	require.Equal(t, "这是最终回答。", reply)
	require.Len(t, model.calls, 1)
	require.Empty(t, model.calls[0].ToolChoice)
	for _, tool := range model.calls[0].Tools {
		if tool.Function != nil {
			require.NotEqual(t, "FinishAgentTurn", tool.Function.Name)
		}
	}
}
