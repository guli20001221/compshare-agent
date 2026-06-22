package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestScenario_UpstreamRetCodeHintFedToModel is the end-to-end wiring guard for
// P0 阶段1B: a known upstream RetCode (230) returned by the executor must reach
// the model's next-round tool result WITH the recovery hint attached. This fails
// if any link in executor → executeWithRetry → ExecuteSafe → executeSafeTool →
// the ReAct error branch flattens the error with %v (which would strip the typed
// *tools.UpstreamAPIError and silently drop the hint).
func TestScenario_UpstreamRetCodeHintFedToModel(t *testing.T) {
	exec := &mockExecutorWithErrors{
		errors: map[string]error{
			"DescribeAvailableCompShareInstanceTypes": tools.NewUpstreamAPIError(230, "Params [Zone] not available"),
		},
	}
	mock := &mockLLM{
		responses: []llm.ChatResponse{
			{ToolCalls: []openai.ToolCall{tc("DescribeAvailableCompShareInstanceTypes", map[string]any{"Zone": "cn-sh2-02"})}},
			{Content: "换一个可用区后再试试。"},
		},
	}

	eng := NewWithDeps(mock, exec, nil)
	eng.Init(context.Background())

	_, err := eng.Chat(context.Background(), "查一下sh2-02的库存", func(StepEvent) {})
	require.NoError(t, err)

	// The round-2 request's last message is the tool result fed back to the model.
	require.Len(t, mock.calls, 2, "expected a follow-up round after the tool error")
	lastMsgs := mock.calls[1].Messages
	toolMsg := lastMsgs[len(lastMsgs)-1].Content

	assert.Contains(t, toolMsg, "API 调用失败", "raw error still surfaced to the model")
	assert.Contains(t, toolMsg, "建议：", "the 阶段1B recovery hint must be attached")
	assert.Contains(t, toolMsg, retCodeHintProbe(), "the 230 hint text must reach the model")
}

// retCodeHintProbe returns a stable substring of the 230 hint so the assertion
// stays meaningful without pinning the whole sentence.
func retCodeHintProbe() string {
	h := tools.NewUpstreamAPIError(230, "x").Hint
	// First clause up to the first Chinese colon is stable guidance text.
	if i := strings.Index(h, "："); i > 0 {
		return h[:i]
	}
	return h
}
