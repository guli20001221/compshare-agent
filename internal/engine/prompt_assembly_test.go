package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const contextCardMarker = "【本轮统一上下文"

// With canonical replay, compaction must not reintroduce a semantic summary
// block. This input has no live execution continuity, so it should carry only
// the base system prompt.
func TestAssembledContextHasNoSemanticMemoryBlockWithCompactionOn(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "好的，继续。"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.SetReactHistoryCompactionEnabled(true)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleUser, Content: "先看看配置"},
		{Role: openai.ChatMessageRoleAssistant, Content: "好的"},
	}

	_, err := eng.Chat(context.Background(), "继续", noopStep)
	require.NoError(t, err)
	require.Len(t, mock.calls, 1)

	sent := mock.calls[0].Messages
	assert.Equal(t, 1, countSystemMessages(sent),
		"compaction must not add a summary when there is no live execution card")
	cardHits := 0
	for _, msg := range sent {
		if msg.Role == openai.ChatMessageRoleSystem && strings.Contains(msg.Content, contextCardMarker) {
			cardHits++
		}
	}
	assert.Equal(t, 0, cardHits, "semantic task state must not manufacture a context card")
}

// buildMessagesForLLM records the assembler's before/after message counts so the
// convergence is observable; a normal turn never trips the conservative cap.
func TestBuildMessagesForLLM_RecordsAssemblyObservability(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "答复。"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)

	_, err := eng.Chat(context.Background(), "你好", noopStep)
	require.NoError(t, err)

	assert.Greater(t, eng.PromptMessagesRawPeak(), 0)
	assert.Greater(t, eng.PromptMessagesAssembledPeak(), 0)
	assert.False(t, eng.PromptMessagesCapApplied(), "a normal turn stays well under the cap")
}

func TestTrimAssembledRequest_UnderBudgetUnchanged(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleSystem, Content: contextCardMarker},
		{Role: openai.ChatMessageRoleUser, Content: "q"},
		{Role: openai.ChatMessageRoleAssistant, Content: "a"},
	}
	out := trimAssembledRequest(msgs, maxAssembledRequestRunes)
	require.Len(t, out, len(msgs))

	// A disabled budget is not the same as a large one, and both must be no-ops.
	require.Len(t, trimAssembledRequest(msgs, 0), len(msgs))
}

func TestTrimAssembledRequest_DropsOldestRecentPairsFirst(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleSystem, Content: contextCardMarker},
		{Role: openai.ChatMessageRoleUser, Content: "p1u"},
		{Role: openai.ChatMessageRoleAssistant, Content: "p1a"},
		{Role: openai.ChatMessageRoleUser, Content: "p2u"},
		{Role: openai.ChatMessageRoleAssistant, Content: "p2a"},
		{Role: openai.ChatMessageRoleUser, Content: "p3u"},
		{Role: openai.ChatMessageRoleAssistant, Content: "p3a"},
		{Role: openai.ChatMessageRoleUser, Content: "current-question"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{toolCall("t1", "DescribeCompShareInstance", `{}`)}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "t1", Content: `{"RetCode":0}`},
	}

	// Exactly the two oldest pairs' worth of room removed — computed from the
	// fixture so that editing a message's text cannot silently change which
	// exchanges the trim is being asked to shed.
	budget := assembledRequestRunes(msgs) - assembledRequestRunes(msgs[2:6])
	out := trimAssembledRequest(msgs, budget)

	require.LessOrEqual(t, assembledRequestRunes(out), budget)
	rendered := renderTestMessages(out)
	assert.NotContains(t, rendered, "p1u", "oldest restored pair is shed first")
	assert.NotContains(t, rendered, "p2u")
	assert.Contains(t, rendered, "p3u", "the most recent restored pair is kept")
	assert.Contains(t, rendered, "current-question", "the current question is always retained")
	assert.Equal(t, "sys", out[0].Content, "the base system prompt is never dropped")
	assert.Equal(t, contextCardMarker, out[1].Content, "the context card is never dropped")
	assertToolCallPairsValid(t, out)
}

func TestTrimAssembledRequest_DropsOldestToolGroupsPreservingPairs(t *testing.T) {
	group := func(id string) []openai.ChatCompletionMessage {
		return []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{toolCall(id, "DescribeCompShareInstance", `{}`)}},
			{Role: openai.ChatMessageRoleTool, ToolCallID: id, Content: `{"RetCode":0,"id":"` + id + `"}`},
		}
	}
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleSystem, Content: contextCardMarker},
		{Role: openai.ChatMessageRoleUser, Content: "current-question"},
	}
	msgs = append(msgs, group("g1")...)
	msgs = append(msgs, group("g2")...)
	msgs = append(msgs, group("g3")...)
	msgs = append(msgs, group("g4")...)

	budget := assembledRequestRunes(msgs) - 2*assembledRequestRunes(group("g1"))
	out := trimAssembledRequest(msgs, budget)

	require.LessOrEqual(t, assembledRequestRunes(out), budget)
	rendered := renderTestMessages(out)
	assert.Contains(t, rendered, "current-question", "the current question is always retained")
	assert.NotContains(t, rendered, `"id":"g1"`, "oldest in-turn tool group is shed first")
	assert.NotContains(t, rendered, `"id":"g2"`)
	assert.Contains(t, rendered, `"id":"g4"`, "the most recent in-turn tool group is kept")
	// No orphaned tool result: the first message after the current question must
	// be an assistant that starts a group, and every tool result has its call.
	afterUser := out[3:]
	require.NotEmpty(t, afterUser)
	assert.Equal(t, openai.ChatMessageRoleAssistant, afterUser[0].Role)
	assertToolCallPairsValid(t, out)
}
