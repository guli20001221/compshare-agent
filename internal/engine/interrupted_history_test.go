package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/opscontext"
	"github.com/compshare-agent/internal/security"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

// Regression for the production request that changed instance/port, was
// interrupted, and then resumed with only "继续". Compile at the real turn-entry
// boundary: the new user has not yet been appended, so every existing user is
// historical even when its text is identical to the current question.
func TestInterruptedUserHistoryReachesOuterAndInnerModels(t *testing.T) {
	const oldTask = "请为 uhost-old 开放 3001 端口"
	const interruptedTask = "uhost-new 对外开放 9000 端口"
	eng := &Engine{messages: []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleUser, Content: oldTask},
		{Role: openai.ChatMessageRoleAssistant, Content: "继续检查 3001。"},
		{Role: openai.ChatMessageRoleUser, Content: interruptedTask},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{toolCall("interrupted", "DiagnoseInstanceInternals", `{}`)}},
		{Role: openai.ChatMessageRoleTool, ToolCallID: "interrupted", Content: "uncommitted raw output"},
		{Role: openai.ChatMessageRoleUser, Content: "继续"},
	}}
	view := (ContextCompiler{}).Compile(eng, "继续", time.Unix(1_750_000_000, 0))
	require.Equal(t, []ConversationPair{
		{User: oldTask, Assistant: "继续检查 3001。"},
		{User: interruptedTask},
		{User: "继续"},
	}, view.RecentConversation)
	eng.lastUserMsg = "继续"
	eng.messages = append(eng.messages, userMsg("继续"))
	outer := messagesFromAgentContext(eng.messages, view, true)
	want := []opscontext.ConversationMessage{
		{Role: "user", Content: oldTask},
		{Role: "assistant", Content: "继续检查 3001。"},
		{Role: "user", Content: interruptedTask},
		{Role: "user", Content: "继续"},
		{Role: "user", Content: "继续"},
	}
	require.Equal(t, want, visibleConversation(outer))
	require.Equal(t, want, eng.instanceOpsConversationHistory())
	require.NotContains(t, renderTestMessages(outer), "uncommitted raw output")
}

func TestInterruptedUserHistoryHotColdAcrossConsecutiveCancellations(t *testing.T) {
	hot := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "已检查旧实例。"}}}, &mockExecutor{}, nil)
	_, err := hot.Chat(context.Background(), "检查 uhost-old 的 3001 端口", noopStep)
	require.NoError(t, err)
	hot.llmClient = &mockLLMWithError{err: context.Canceled}
	for _, question := range []string{"检查 uhost-new 的 9000 端口", "继续", "继续"} {
		_, err = hot.Chat(context.Background(), question, noopStep)
		require.ErrorIs(t, err, context.Canceled)
	}
	var rows []HistoryMessage
	for _, message := range hot.MessagesSnapshot() {
		if message.Role == openai.ChatMessageRoleUser || message.Role == openai.ChatMessageRoleAssistant && len(message.ToolCalls) == 0 {
			rows = append(rows, HistoryMessage{Role: message.Role, Content: historyConversationText(message.Role, message.Content)})
		}
	}
	cold := NewWithDeps(nil, &mockExecutor{}, nil)
	cold.RehydrateHistory(rows)
	want := []opscontext.ConversationMessage{
		{Role: "user", Content: "检查 uhost-old 的 3001 端口"},
		{Role: "assistant", Content: "已检查旧实例。"},
		{Role: "user", Content: "检查 uhost-new 的 9000 端口"},
		{Role: "user", Content: "继续"},
		{Role: "user", Content: "继续"},
		{Role: "user", Content: "继续"},
	}
	for _, eng := range []*Engine{hot, cold} {
		mock := &mockLLM{responses: []llm.ChatResponse{{Content: "继续检查新实例。"}}}
		eng.llmClient = mock
		_, err = eng.Chat(context.Background(), "继续", noopStep)
		require.NoError(t, err)
		require.Len(t, mock.calls, 1)
		require.Equal(t, want, visibleConversation(mock.calls[0].Messages))
	}
}

func TestInterruptedUserHistoryBudgetKeepsWholeRecentEndpoints(t *testing.T) {
	eng := &Engine{messages: []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleUser, Content: "old"},
		{Role: openai.ChatMessageRoleAssistant, Content: strings.Repeat("旧", maxRawHistoryRunes)},
		{Role: openai.ChatMessageRoleUser, Content: strings.Repeat("新", maxReplayedHistoryRunes-2)},
		{Role: openai.ChatMessageRoleUser, Content: "继续"},
	}}
	view := (ContextCompiler{}).Compile(eng, "继续", time.Now())
	require.Len(t, view.RecentConversation, 2)
	require.Equal(t, maxReplayedHistoryRunes, conversationPairsRunes(view.RecentConversation))
	eng.trimHistory()
	require.Equal(t, view.RecentConversation, (ContextCompiler{}).Compile(eng, "继续", time.Now()).RecentConversation)

	// A one-rune overflow drops the entire older endpoint, never a fragment or a
	// fabricated assistant. A sole oversized latest user still follows the
	// existing newest-exchange retention rule until the final request ceiling.
	eng.messages = append(eng.messages, userMsg("再"))
	view = (ContextCompiler{}).Compile(eng, "继续", time.Now())
	require.Equal(t, []ConversationPair{{User: "继续"}, {User: "再"}}, view.RecentConversation)
	eng.messages = append(eng.messages, userMsg(strings.Repeat("末", maxReplayedHistoryRunes+1)))
	view = (ContextCompiler{}).Compile(eng, "继续", time.Now())
	require.Len(t, view.RecentConversation, 1)
	require.Equal(t, maxReplayedHistoryRunes+1, conversationPairsRunes(view.RecentConversation))
}

func TestInterruptedUserHistoryKeepsTranscriptAttribution(t *testing.T) {
	eng := &Engine{messages: []openai.ChatCompletionMessage{paritySystemMessage()}}
	turn := func(evidence string) []openai.ChatCompletionMessage {
		return []openai.ChatCompletionMessage{
			userMsg("继续"),
			assistantCalls(toolCall(evidence, "DescribeCompShareInstance", `{}`)),
			toolMsg(evidence, evidence),
			finalMsg("已确认。"),
		}
	}
	finishTurn(eng, turn("old-instance-evidence"))
	eng.messages = append(eng.messages, userMsg("继续"))
	finishTurn(eng, turn("new-instance-evidence"))
	view := (ContextCompiler{}).Compile(eng, "继续", time.Now())
	require.Len(t, view.RecentConversation, 3)
	require.Contains(t, renderTestMessages(view.RecentConversation[0].Transcript), "old-instance-evidence")
	require.Equal(t, ConversationPair{User: "继续"}, view.RecentConversation[1])
	require.Contains(t, renderTestMessages(view.RecentConversation[2].Transcript), "new-instance-evidence")
}

func TestInterruptedUserHistoryBridgeAnchorWithRepeatedContinuation(t *testing.T) {
	wrapped := WrapScreenshotContext("CUDA failure\ncontact ocr@example.com", "检查新实例\nAuthorization: Bearer fixture-secret")
	hot := &Engine{lastUserMsg: "继续", messages: []openai.ChatCompletionMessage{
		userMsg(wrapped), userMsg("继续"),
	}}
	first := hot.instanceOpsConversationHistory()
	require.Len(t, first, 2)
	anchor := opscontext.ConversationAnchor(first)

	cold := &Engine{lastUserMsg: "继续"}
	cold.RehydrateHistory([]HistoryMessage{
		{Role: "user", Content: security.RedactUserConversationText(wrapped)},
		{Role: "user", Content: "继续"},
	})
	for _, eng := range []*Engine{hot, cold} {
		eng.messages = append(eng.messages, userMsg("继续"))
		history := eng.instanceOpsConversationHistory()
		require.Len(t, history, 3)
		delta, ok := opscontext.ConversationAfterAnchor(history, anchor)
		require.True(t, ok)
		require.Equal(t, []opscontext.ConversationMessage{{Role: "user", Content: "继续"}}, delta)
		require.NotContains(t, history[0].Content, "fixture-secret")
		require.NotContains(t, history[0].Content, "ocr@example.com")
		require.Contains(t, history[0].Content, "CUDA failure")
	}
}

func TestInterruptedUserHistoryBridgeBudgetIncludesCurrentUser(t *testing.T) {
	eng := &Engine{lastUserMsg: "继续", messages: []openai.ChatCompletionMessage{
		userMsg("older"), finalMsg("answer"),
		userMsg(strings.Repeat("新", maxReplayedHistoryRunes-2)),
		userMsg("继续"),
	}}
	history := eng.instanceOpsConversationHistory()
	require.Len(t, history, 2)
	require.Equal(t, maxReplayedHistoryRunes, len([]rune(history[0].Content))+len([]rune(history[1].Content)))
	eng.messages = append(eng.messages, userMsg("继续"))
	history = eng.instanceOpsConversationHistory()
	require.Equal(t, []opscontext.ConversationMessage{{Role: "user", Content: "继续"}, {Role: "user", Content: "继续"}}, history)
}

func TestConversationProjectionDoesNotAttachOrphanAssistant(t *testing.T) {
	require.Equal(t, []ConversationPair{{User: "interrupted"}, {User: "answered", Assistant: "final"}}, conversationPairsFromMessages([]openai.ChatCompletionMessage{
		finalMsg("orphan before history"),
		userMsg("interrupted"),
		userMsg(" "),
		finalMsg("orphan after blank user"),
		userMsg("answered"),
		finalMsg("final"),
		finalMsg("orphan after final"),
	}))
}

func visibleConversation(messages []openai.ChatCompletionMessage) []opscontext.ConversationMessage {
	var out []opscontext.ConversationMessage
	for _, message := range messages {
		if message.Role == openai.ChatMessageRoleUser || message.Role == openai.ChatMessageRoleAssistant && len(message.ToolCalls) == 0 {
			out = append(out, opscontext.ConversationMessage{Role: message.Role, Content: message.Content})
		}
	}
	return out
}
