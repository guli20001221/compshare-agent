package engine

import (
	openai "github.com/sashabaranov/go-openai"
)

// stripHistoricalToolTranscript keeps the plain conversation history small and
// makes hot history match cold reconstruction. Recorded tool turns are attached
// later as whole exchanges by the canonical transcript replay.
func stripHistoricalToolTranscript(messages []openai.ChatCompletionMessage) []openai.ChatCompletionMessage {
	if len(messages) == 0 {
		return messages
	}
	out := make([]openai.ChatCompletionMessage, 0, len(messages))
	for _, msg := range messages {
		if msg.Role == openai.ChatMessageRoleTool {
			continue
		}
		if msg.Role == openai.ChatMessageRoleAssistant && len(msg.ToolCalls) > 0 {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func safeHistoryStart(messages []openai.ChatCompletionMessage, candidateStart int) int {
	if candidateStart <= 1 {
		return -1
	}
	for i := candidateStart; i < len(messages); i++ {
		msg := messages[i]
		if msg.Role == openai.ChatMessageRoleUser {
			return i
		}
		if msg.Role == openai.ChatMessageRoleAssistant && len(msg.ToolCalls) == 0 {
			return i
		}
	}
	return -1
}
