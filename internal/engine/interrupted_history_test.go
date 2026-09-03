package engine

import (
	"context"
	"encoding/json"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/opscontext"
	"github.com/compshare-agent/internal/security"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
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
		require.Contains(t, history[0].Content, "ocr@example.com")
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

type cancelAfterReadLLM struct {
	cancel context.CancelFunc
	stage  int
	calls  []llm.ChatRequest
}

func (m *cancelAfterReadLLM) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	m.calls = append(m.calls, req)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.stage++
	switch m.stage {
	case 1:
		return &llm.ChatResponse{ToolCalls: []openai.ToolCall{
			toolCall("observed-before-cancel", "ReadCapability_resource_info", `{}`),
		}}, nil
	case 2:
		m.cancel()
		return nil, ctx.Err()
	default:
		return &llm.ChatResponse{Content: "我看到了上轮的查询结果。"}, nil
	}
}

func conversationOnly(messages []openai.ChatCompletionMessage) []openai.ChatCompletionMessage {
	var result []openai.ChatCompletionMessage
	for _, message := range messages {
		if message.Role != openai.ChatMessageRoleSystem {
			result = append(result, message)
		}
	}
	return result
}

func TestInterruptedReadTurnReplaysObservedResultsOnHotAndColdContinuation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := &cancelAfterReadLLM{cancel: cancel}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"UHostSet": []any{map[string]any{
			"UHostId": "uhost-interrupted", "Name": "observed-instance", "State": "Running",
		}}},
	}}
	hot := NewWithDeps(model, executor, nil)
	hot.RehydrateHistory(nil)
	const question = "查看我的实例状态"
	_, err := hot.Chat(ctx, question, noopStep)
	require.ErrorIs(t, err, context.Canceled)
	require.Contains(t, executor.calls, "DescribeCompShareInstance", "the read completed before the model was cancelled")
	executedBeforeContinuation := append([]string(nil), executor.calls...)
	metadata, stats := hot.LastTurnTranscript()
	require.True(t, stats.Attempted)
	require.NotEmpty(t, metadata)
	require.Len(t, hot.recentTurns, 1)
	require.Empty(t, hot.recentTurns[0].Assistant, "no successful answer was committed")

	coldModel := &mockLLM{responses: []llm.ChatResponse{{Content: "我看到了上轮的查询结果。"}}}
	cold := NewWithDeps(coldModel, &mockExecutor{}, nil)
	cold.RehydrateHistory([]HistoryMessage{
		{Role: openai.ChatMessageRoleUser, Content: question},
		{Role: openai.ChatMessageRoleAssistant, Transcript: metadata},
	})
	before := len(model.calls)
	_, err = hot.Chat(context.Background(), "继续刚才的问题", noopStep)
	require.NoError(t, err)
	_, err = cold.Chat(context.Background(), "继续刚才的问题", noopStep)
	require.NoError(t, err)
	hotRequest := conversationOnly(model.calls[before].Messages)
	coldRequest := conversationOnly(coldModel.calls[0].Messages)
	require.Equal(t, hotRequest, coldRequest)
	requireProviderLegal(t, hotRequest)
	require.Contains(t, renderTestMessages(hotRequest), "observed-instance")
	require.Equal(t, question, hotRequest[0].Content)
	require.Equal(t, executedBeforeContinuation, executor.calls, "replaying historical observations must not rerun the tool")
	for _, message := range hotRequest {
		require.False(t, message.Role == openai.ChatMessageRoleAssistant && message.Content == "" && len(message.ToolCalls) == 0)
	}
}

func TestInterruptedHistoryKeepsEveryUnansweredUserWithoutInventingAnswer(t *testing.T) {
	for _, terminalRow := range []bool{false, true} {
		history := []HistoryMessage{{Role: "user", Content: "uhost-first 请求还未完成"}}
		if terminalRow {
			history = append(history, HistoryMessage{Role: "assistant"})
		}
		history = append(history, HistoryMessage{Role: "user", Content: "别改目标，继续检查"})
		eng := &Engine{}
		eng.RehydrateHistory(history)
		request := assembleNextTurn(eng, "怎么样了")
		conversation := conversationOnly(request)
		require.Len(t, conversation, 3)
		require.Equal(t, "uhost-first 请求还未完成", conversation[0].Content)
		require.Equal(t, "别改目标，继续检查", conversation[1].Content)
		require.Equal(t, "怎么样了", conversation[2].Content)
		for _, message := range conversation {
			require.Equal(t, openai.ChatMessageRoleUser, message.Role)
		}
	}
}

func TestTransportCancellationDiscardsOnlyUncommittedAnswer(t *testing.T) {
	turn := []openai.ChatCompletionMessage{
		{Role: "user", Content: "查询实例"},
		{Role: "assistant", ToolCalls: []openai.ToolCall{toolCall("read", "ReadCapability_resource_info", `{}`)}},
		{Role: "tool", ToolCallID: "read", Content: `{"state":"Running"}`},
		{Role: "assistant", Content: "这个未交付的回答不能视为完成。"},
	}
	hot, _, _ := runHotTurn(turn)
	hot.MarkLastTurnInterrupted()
	metadata, _ := hot.LastTurnTranscript()
	cold := rebuildCold("查询实例", "", metadata)
	require.Len(t, hot.recentTurns, 1, "reconciliation replaces the prior record instead of duplicating it")
	require.Empty(t, hot.recentTurns[0].Assistant)
	hotRequest := assembleNextTurn(hot, "继续")
	coldRequest := assembleNextTurn(cold, "继续")
	require.Equal(t, hotRequest, coldRequest)
	require.Contains(t, renderReplayedRegion(t, hotRequest), "Running")
	require.NotContains(t, renderTestMessages(hotRequest), "这个未交付的回答")
	requireProviderLegal(t, hotRequest)
}

func TestCanonicalTranscriptKeepsThreeFourThousandRuneChunkBodies(t *testing.T) {
	chunks := []map[string]string{
		{"chunk_id": "first", "content": strings.Repeat("甲", 3995) + "END_1"},
		{"chunk_id": "second", "content": strings.Repeat("乙", 3995) + "END_2"},
		{"chunk_id": "third", "content": strings.Repeat("丙", 3995) + "END_3"},
	}
	body, err := json.Marshal(map[string]any{"EvidenceLedger": map[string]any{"items": chunks}, "complete": true})
	require.NoError(t, err)
	_, metadata, stats := runHotTurn([]openai.ChatCompletionMessage{
		{Role: "user", Content: "读取三个文档段落"},
		{Role: "assistant", ToolCalls: []openai.ToolCall{toolCall("chunks", "ReadChunk", `{"chunk_ids":["first","second","third"]}`)}},
		{Role: "tool", ToolCallID: "chunks", Content: string(body)},
		{Role: "assistant", Content: "已读取。"},
	})
	require.False(t, stats.Oversized)
	require.LessOrEqual(t, len(metadata), maxTranscriptBytes)
	transcript := ParseTranscriptMetadata(metadata)
	require.NotNil(t, transcript)
	require.Equal(t, 0, transcript.DroppedRounds)
	require.LessOrEqual(t, transcriptRunes(transcript.Messages), maxTranscriptTotalRunes)
	result := ProjectTranscript(transcript)[2].Content
	require.JSONEq(t, string(body), result)
	for _, tail := range []string{"END_1", "END_2", "END_3"} {
		require.Contains(t, result, tail)
	}
}
