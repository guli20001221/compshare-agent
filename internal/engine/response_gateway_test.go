package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

// TestRequiredEvidenceIsPrependedWhenTheAgentOmitsItsPlaceholder covers the
// force-insert mechanism itself, at the gateway, rather than through whichever
// capability happens to be classified `required` this week. resource_info was
// that capability and no longer is (a targeted question does not want the whole
// instance list stapled on), so without this the guarantee would have lost its
// only test when the classification changed.
func TestRequiredEvidenceIsPrependedWhenTheAgentOmitsItsPlaceholder(t *testing.T) {
	evidence := []readResponseEvidence{
		{Capability: "stock_availability", Reply: "4090 华北二A 余量 3", Placeholder: "{{READ_OBSERVATION_1}}", Required: true},
		{Capability: "image_list", Reply: "目录若干", Placeholder: "{{READ_OBSERVATION_2}}"},
	}
	got := substituteReadObservationBlocks("库存我看过了。", evidence)
	require.Equal(t, "4090 华北二A 余量 3\n\n库存我看过了。", got,
		"a required block the Agent left out must still reach the user")

	// The optional one is not prepended — only substituted where it was asked for.
	require.Equal(t, "见 目录若干 。",
		substituteReadObservationBlocks("见 {{READ_OBSERVATION_2}} 。", evidence[1:]))
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

func TestResponseGatewaySubstitutesOnlyAgentSelectedObservationBlocks(t *testing.T) {
	evidence := []readResponseEvidence{
		{Reply: "精确实例表", Placeholder: "{{READ_OBSERVATION_1}}"},
		{Reply: "精确价格表", Placeholder: "{{READ_OBSERVATION_2}}"},
	}
	require.Equal(t, "结论如下：\n精确价格表", substituteReadObservationBlocks("结论如下：\n{{READ_OBSERVATION_2}}", evidence))
	require.Equal(t, "只做解释，不展示精确表格。", substituteReadObservationBlocks("只做解释，不展示精确表格。", evidence))
}

func TestResponseGatewayInsertsMissingRequiredObservationBlock(t *testing.T) {
	evidence := []readResponseEvidence{{
		Reply: "opaque token", Placeholder: "{{READ_OBSERVATION_1}}", Required: true,
	}}
	require.Equal(t,
		"opaque token\n\nToken 已获取。",
		substituteReadObservationBlocks("Token 已获取。", evidence))
	require.Equal(t,
		"opaque token",
		substituteReadObservationBlocks("{{READ_OBSERVATION_1}}", evidence))
}

func TestResponseGatewayNeverShipsToolProtocolMarkup(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	reply := eng.finalizeResponse(context.Background(), "给实例重置密码",
		`<｜DSML｜invoke name="RequestResetPassword">{"Password":"Secret123!"}`)
	require.Equal(t, malformedToolProtocolReply, reply)
	require.NotContains(t, reply, "Secret123!")
	require.NotContains(t, reply, "DSML")
}
