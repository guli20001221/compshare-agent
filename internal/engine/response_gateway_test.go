package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/agentprotocol"
	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

// Sensitive credentials stay outside model context and are the only read result
// the server composes into the final response.
func TestSensitiveReplyIsPrependedWithoutAnyAgentReference(t *testing.T) {
	got := prependSensitiveReplies("已获取访问凭据。", []string{"Jupyter Token：opaque-token"})
	require.Equal(t, "Jupyter Token：opaque-token\n\n已获取访问凭据。", got,
		"the server delivers a secret once without exposing it to the Agent")
	require.Equal(t, "普通答案。", prependSensitiveReplies("普通答案。", nil))
}

// TestResourceInfoDoesNotStapleTheWholeListOntoATargetedAnswer is the measured
// regression: with resource_info forced, 「我那台 4090 的内存是多少」 got all ten
// instances (three of them 5090s) above the answer, because the rendered block
// is the whole list whenever the Agent queried without targets.
func TestResourceInfoDoesNotStapleTheWholeListOntoATargetedAnswer(t *testing.T) {
	model := &streamingSeqMockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("read", capability.ReadToolName(intent.IntentResourceInfo), `{}`)}},
		{Content: "你有一台名为 train-a 的实例（uhost-1），当前正在运行。"},
	}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"TotalCount": float64(1), "UHostSet": []any{map[string]any{
			// Memory is MB upstream: 65536 MB is the 64 GB this fixture means.
			"UHostId": "uhost-1", "Name": "train-a", "State": "Running", "GpuType": "4090", "GPU": float64(1), "CPU": float64(8), "Memory": float64(65536),
		}}},
	}}
	eng := NewWithDeps(model, executor, nil)

	var deltas []string
	reply, err := eng.ChatWithOptions(context.Background(), "我有哪些实例？", noopStep, ChatOptions{
		OnTextDelta: func(delta string) { deltas = append(deltas, delta) },
	})
	require.NoError(t, err)
	require.Equal(t, "你有一台名为 train-a 的实例（uhost-1），当前正在运行。", reply,
		"the Agent did not ask for the block; a targeted answer must not grow the list")
	streamed := strings.Join(deltas, "")
	require.Equal(t, reply, streamed)
}

func TestResponseGatewayDoesNotOverrideConversationOnlyAnswer(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	require.Equal(t, "结合上一轮的回答继续即可。", eng.finalizeResponse(context.Background(), "那继续呢", "结合上一轮的回答继续即可。"))
}

func TestResponseGatewayKeepsThePromptedConsoleMarker(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.feishuConsoleHandoffThisTurn = true
	eng.searchKnowledgeRanThisTurn = true
	eng.knowledgeQAAgentLoopThisTurn = true

	reply := eng.finalizeResponse(context.Background(), "谁能帮忙处理？", agentprotocol.FeishuConsoleHandoffMarker)

	require.Equal(t, agentprotocol.FeishuConsoleHandoffMarker, reply,
		"the prompted console marker is a legal completion, not an unknown [[chunk_id]]")
}

func TestResponseGatewayNeverShipsTheConsoleMarkerToOrdinaryWeb(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	reply := eng.finalizeResponse(context.Background(), "谁能帮忙处理？", agentprotocol.FeishuConsoleHandoffMarker)

	require.NotContains(t, reply, agentprotocol.FeishuConsoleHandoffMarker)
	require.Equal(t, emptyReplyFallbackMessage, reply)
}

func TestResponseGatewayDoesNotAcceptAModelAuthoredCustomerSupportMarker(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	reply := eng.finalizeResponse(context.Background(), "继续回答", agentprotocol.FeishuCustomerSupportMarker)
	require.NotContains(t, reply, agentprotocol.FeishuCustomerSupportMarker)
	require.Equal(t, emptyReplyFallbackMessage, reply)
}

func TestResponseGatewayNeverShipsToolProtocolMarkup(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	reply := eng.finalizeResponse(context.Background(), "给实例重置密码",
		`<｜DSML｜invoke name="RequestResetPassword">{"Password":"Secret123!"}`)
	require.Equal(t, malformedToolProtocolReply, reply)
	require.NotContains(t, reply, "Secret123!")
	require.NotContains(t, reply, "DSML")
}

func TestResponseGatewayLetsTheUserCopyOnlyTheirCurrentSignedURL(t *testing.T) {
	const signedURL = "https://civitai.example/download?Authorization=signed-token-abcdefghijklmnopqrst"
	user := "请给这个链接生成下载命令：" + signedURL
	draft := "执行：curl -L '" + signedURL + "' -o model.safetensors"
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)

	reply := eng.finalizeResponse(context.Background(), user, draft)

	require.Equal(t, draft, reply)
}
