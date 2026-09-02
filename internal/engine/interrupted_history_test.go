package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

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
