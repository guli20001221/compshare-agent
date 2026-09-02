package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	openai "github.com/sashabaranov/go-openai"
)

func TestTrimHistoryKeepsOnlyPlainConversationAndFitsTheRawBudget(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	const padRunes = 400
	pairs := overflowingPairs(padRunes)
	eng.messages = makePaddedHistory(pairs, padRunes)
	// A historic tool exchange must never survive in e.messages. Its canonical
	// transcript is stored separately and replayed as a complete exchange.
	eng.messages = append(eng.messages,
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "tool question"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{toolCall("old-tool", "DescribeCompShareInstance", `{}`)}},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "old-tool", Content: `{"secret":"must-not-stay"}`},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "tool answer"},
	)
	require.Greater(t, assembledRequestRunes(eng.messages[1:]), maxRawHistoryRunes,
		"premise: trim must have work to do")

	eng.trimHistory()

	require.LessOrEqual(t, assembledRequestRunes(eng.messages[1:]), maxRawHistoryRunes)
	assert.Equal(t, openai.ChatMessageRoleSystem, eng.messages[0].Role)
	assert.Equal(t, 1, countSystemMessages(eng.messages))
	for _, message := range eng.messages {
		assert.NotEqual(t, openai.ChatMessageRoleTool, message.Role)
		assert.Empty(t, message.ToolCalls)
		assert.NotContains(t, message.Content, "must-not-stay")
	}
}

func TestRawHistoryBudgetDoesNotNarrowReplayableConversation(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	const padRunes = 400
	pairs := 2 * overflowingPairs(padRunes)
	eng.messages = makePaddedHistory(pairs, padRunes)
	require.Greater(t, assembledRequestRunes(eng.messages[1:]), maxRawHistoryRunes)

	before := eng.recentConversationPairs()
	require.NotEmpty(t, before)
	require.Less(t, len(before), pairs, "premise: the replay budget must be active")

	eng.trimHistory()
	require.Less(t, len(eng.messages), 1+2*pairs, "premise: raw trimming must occur")
	assert.Equal(t, before, eng.recentConversationPairs(),
		"the source-history budget may not remove an exchange the replay budget kept")
}

func makePaddedHistory(pairs, padRunes int) []openai.ChatCompletionMessage {
	msgs := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "system prompt"}}
	for i := 0; i < pairs; i++ {
		msgs = append(msgs,
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: fmt.Sprintf("q%d", i) + strings.Repeat("问", padRunes)},
			openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: fmt.Sprintf("a%d", i) + strings.Repeat("答", padRunes)},
		)
	}
	return msgs
}

func overflowingPairs(padRunes int) int {
	if padRunes <= 0 {
		panic("padRunes must be positive")
	}
	return maxRawHistoryRunes/(2*padRunes) + 2
}

func countSystemMessages(messages []openai.ChatCompletionMessage) int {
	count := 0
	for _, message := range messages {
		if message.Role == openai.ChatMessageRoleSystem {
			count++
		}
	}
	return count
}

func assertToolCallPairsValid(t *testing.T, messages []openai.ChatCompletionMessage) {
	t.Helper()
	toolResponses := map[string]bool{}
	for _, message := range messages {
		if message.Role == openai.ChatMessageRoleTool {
			toolResponses[message.ToolCallID] = true
		}
	}
	for _, message := range messages {
		if message.Role != openai.ChatMessageRoleAssistant {
			continue
		}
		for _, call := range message.ToolCalls {
			assert.Truef(t, toolResponses[call.ID], "tool_call %s must have a matching tool response", call.ID)
		}
	}
}
