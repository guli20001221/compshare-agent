package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

func TestCentralResponseGatewaySubmitsServerReadReplyInsteadOfModelRetyping(t *testing.T) {
	model := &streamingSeqMockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("read", tools.ReadPlatformCapabilityName, `{"capability":"resource_info","slots":{}}`)}},
		{Content: "你有 15 台实例，其中 uhost-invented 正在运行。"},
	}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"TotalCount": float64(1), "UHostSet": []any{map[string]any{
			"UHostId": "uhost-1", "Name": "train-a", "State": "Running", "GpuType": "4090", "GPU": float64(1), "CPU": float64(8), "Memory": float64(64),
		}}},
	}}
	eng := NewWithDeps(model, executor, nil)
	eng.SetCentralAgentRuntimeEnabled(true)

	var deltas []string
	reply, err := eng.ChatWithOptions(context.Background(), "我有哪些实例？", noopStep, ChatOptions{
		OnTextDelta: func(delta string) { deltas = append(deltas, delta) },
	})
	require.NoError(t, err)
	require.Contains(t, reply, "uhost-1")
	require.NotContains(t, reply, "uhost-invented")
	require.NotContains(t, reply, "15 台")
	streamed := strings.Join(deltas, "")
	require.Equal(t, reply, streamed)
	require.NotContains(t, streamed, "uhost-invented")
}

func TestResponseGatewayDoesNotOverrideConversationOnlyAnswer(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetCentralAgentRuntimeEnabled(true)
	require.Equal(t, "结合上一轮的回答继续即可。", eng.finalizeResponse(context.Background(), "那继续呢", "结合上一轮的回答继续即可。"))
}

func TestCanonicalReadResponseIsCompleteAndStableForMultipleCapabilities(t *testing.T) {
	got := canonicalReadResponse([]readResponseEvidence{{Reply: "规格事实"}, {Reply: "库存事实"}, {Reply: "规格事实"}})
	require.Equal(t, "规格事实\n\n库存事实", got)
}
