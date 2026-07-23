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

// The whole point of P2: even with history compaction ON (the production
// default that used to inject a second "会话摘要" system block), the model now
// receives exactly ONE structured-memory block — the context card — and the
// task memory appears once, not duplicated across two overlapping blocks.
func TestAssembledContext_SingleMemoryBlock_WithCompactionOn(t *testing.T) {
	mock := &mockLLM{responses: []llm.ChatResponse{{Content: "好的，继续。"}}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.SetReactHistoryCompactionEnabled(true)
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		TaskSnapshot: TaskSnapshot{
			Goal:   "把训练机扩容到 200G",
			Status: TaskSnapshotStatusActive,
		},
		ConversationDigest: ConversationDigest{Narrative: "用户在配置训练机"},
	}, 1)
	eng.messages = []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleUser, Content: "先看看配置"},
		{Role: openai.ChatMessageRoleAssistant, Content: "好的"},
	}

	_, err := eng.Chat(context.Background(), "继续", noopStep)
	require.NoError(t, err)
	require.Len(t, mock.calls, 1)

	sent := mock.calls[0].Messages
	// Before P2 this turn carried three system blocks (base prompt + the
	// 会话摘要 compaction summary + the context card); the summary duplicated the
	// card. Now there are exactly two: the base prompt and the single context
	// card. (Redundancy *within* the card — task goal echoed by the digest — is a
	// separate, pre-existing concern, not the summary-vs-card duplication P2 removes.)
	assert.Equal(t, 2, countSystemMessages(sent),
		"exactly one memory block (the context card) beyond the base system prompt")
	cardHits := 0
	for _, msg := range sent {
		if msg.Role == openai.ChatMessageRoleSystem && strings.Contains(msg.Content, contextCardMarker) {
			cardHits++
		}
	}
	assert.Equal(t, 1, cardHits, "the context card must appear exactly once")
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

func TestCapAssembledRequestMessages_UnderCapUnchanged(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys"},
		{Role: openai.ChatMessageRoleSystem, Content: contextCardMarker},
		{Role: openai.ChatMessageRoleUser, Content: "q"},
		{Role: openai.ChatMessageRoleAssistant, Content: "a"},
	}
	out := capAssembledRequestMessages(msgs, 100)
	require.Len(t, out, len(msgs))
}

func TestCapAssembledRequestMessages_DropsOldestRecentPairsFirst(t *testing.T) {
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

	out := capAssembledRequestMessages(msgs, 8)

	require.LessOrEqual(t, len(out), 8)
	rendered := renderTestMessages(out)
	assert.NotContains(t, rendered, "p1u", "oldest restored pair is shed first")
	assert.NotContains(t, rendered, "p2u")
	assert.Contains(t, rendered, "p3u", "the most recent restored pair is kept")
	assert.Contains(t, rendered, "current-question", "the current question is always retained")
	assert.Equal(t, "sys", out[0].Content, "the base system prompt is never dropped")
	assert.Equal(t, contextCardMarker, out[1].Content, "the context card is never dropped")
	assertToolCallPairsValid(t, out)
}

func TestCapAssembledRequestMessages_DropsOldestToolGroupsPreservingPairs(t *testing.T) {
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

	out := capAssembledRequestMessages(msgs, 7)

	require.LessOrEqual(t, len(out), 7)
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
