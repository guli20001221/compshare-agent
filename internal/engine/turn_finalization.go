package engine

import (
	"context"
	"strings"

	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
)

// finishAgentTurn closes the existing conversation after its tool budget or
// model attempt ends. It preserves the ordinary prompt, task and transcript;
// no tools are offered, so this call can only explain results already obtained.
func (e *Engine) finishAgentTurn(ctx context.Context) (string, bool) {
	if ctx.Err() != nil || e.llmClient == nil || !e.turnReturnedToolResults() {
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

// turnReturnedToolResults reports whether this turn already put a tool result in
// front of the model. The conversation is the authority for that question, not
// the reuse cache: the cache answers whether an identical later call may replay
// an observation, so it deliberately omits unavailable ones — which the closing
// answer still has to report as the part that did not complete.
func (e *Engine) turnReturnedToolResults() bool {
	start := currentTurnStart(e.messages)
	if start < 0 {
		return false
	}
	for _, message := range e.messages[start:] {
		if message.Role == openai.ChatMessageRoleTool {
			return true
		}
	}
	return false
}
