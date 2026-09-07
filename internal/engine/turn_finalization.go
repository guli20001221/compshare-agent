package engine

import (
	"context"
	"strings"

	"github.com/compshare-agent/internal/llm"
)

// finishAgentTurn closes the existing conversation after its tool budget or
// model attempt ends. It preserves the ordinary prompt, task and transcript;
// no tools are offered, so this call can only explain results already obtained.
func (e *Engine) finishAgentTurn(ctx context.Context) (string, bool) {
	if ctx.Err() != nil || e.llmClient == nil || len(e.toolResultsByCallThisTurn) == 0 {
		return "", false
	}
	messages := withEphemeralSystemBeforeLastUser(e.buildMessagesForLLM(nil),
		"本轮工具调用已结束。根据完整对话和已获得的结果直接回答，未完成的部分如实说明。")
	resp, err := e.llmClient.Chat(ctx, llm.ChatRequest{Messages: messages})
	if err != nil || resp == nil {
		return "", false
	}
	e.emitTokenUsage(resp.Usage)
	if resp.OutputIncomplete() || len(resp.ToolCalls) != 0 {
		return "", false
	}
	answer := strings.TrimSpace(resp.Content)
	return answer, answer != ""
}
