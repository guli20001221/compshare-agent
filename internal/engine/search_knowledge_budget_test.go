package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchKnowledgeMainAgentOwnsContextAndFollowUpQueries(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("clipboard", "SearchKnowledge", `{"query":"Windows noVNC 剪贴板支持","context_hint":"浏览器连接 Windows 实例"}`)}},
		{ToolCalls: []openai.ToolCall{toolCall("alternative", "SearchKnowledge", `{"query":"Windows 远程桌面客户端剪贴板"}`)}},
		{Content: "浏览器和客户端的剪贴板支持分别见资料 [[browser-clipboard]] [[client-clipboard]]。"},
	}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{
		{Enabled: true, HitItems: []knowledge.RetrievalHit{{Kept: true, Score: 90, Chunk: knowledge.KBChunk{
			ChunkID: "browser-clipboard", Title: "浏览器剪贴板", Content: "浏览器连接 Windows 实例的剪贴板说明。",
		}}}},
		{Enabled: true, HitItems: []knowledge.RetrievalHit{{Kept: true, Score: 90, Chunk: knowledge.KBChunk{
			ChunkID: "client-clipboard", Title: "客户端剪贴板", Content: "Windows 远程桌面客户端的剪贴板说明。",
		}}}},
	}}
	eng := NewWithDeps(model, &mockExecutor{}, nil)
	eng.InitWithContext("test")
	eng.SetKnowledgeRetriever(retriever)
	eng.messages = append(eng.messages,
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "我是从浏览器打开的 Windows 实例"},
		openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: "你使用的是浏览器 noVNC 连接。"},
	)

	reply, err := eng.Chat(context.Background(), "粘贴呢", noopStep)
	require.NoError(t, err)
	require.Len(t, model.calls, 3, "only the main Agent searches, observes results and answers; no internal planning call")
	assert.Contains(t, fmt.Sprint(model.calls[0].Messages), "我是从浏览器打开的 Windows 实例")
	assert.Contains(t, fmt.Sprint(model.calls[0].Messages), "粘贴呢")
	assert.Contains(t, fmt.Sprint(model.calls[1].Messages), "browser-clipboard", "the next query is chosen with the first result in the canonical transcript")
	assert.Equal(t, []knowledgeRetrievalCall{
		{question: "Windows noVNC 剪贴板支持", productArea: "浏览器连接 Windows 实例"},
		{question: "Windows 远程桌面客户端剪贴板"},
	}, retriever.calls, "execute exactly the queries the main Agent selected")
	assert.Equal(t, 2, eng.searchKnowledgeCallsThisTurn)
	assert.Equal(t, "Windows noVNC 剪贴板支持", eng.searchKnowledgeLedgerThisTurn.Query)
	assert.Len(t, eng.searchKnowledgeLedgerThisTurn.Items, 2)
	assert.NotContains(t, reply, "[[", "citation display behavior remains unchanged")
}

func TestSearchKnowledgeMainAgentBudgetPreservesAllAllowedCalls(t *testing.T) {
	queries := []string{"实例关机计费", "系统盘关机计费", "数据盘关机计费", "云存储关机计费"}
	calls := make([]openai.ToolCall, 0, len(queries))
	for _, query := range queries {
		calls = append(calls, toolCall(query, "SearchKnowledge", `{"query":"`+query+`"}`))
	}
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: calls},
		{Content: "以上查询已完成。"},
	}}
	retriever := &scriptedKnowledgeRetriever{}
	eng := NewWithDeps(model, &mockExecutor{}, nil)
	eng.InitWithContext("test")
	eng.SetKnowledgeRetriever(retriever)

	_, err := eng.Chat(context.Background(), "实例关机后，各项资源还会扣费吗", noopStep)
	require.NoError(t, err)
	require.Len(t, model.calls, 2)
	require.Len(t, retriever.calls, maxSearchKnowledgeCallsPerTurn)
	for i, query := range queries {
		assert.Equal(t, query, retriever.calls[i].question)
	}
	assert.Contains(t, toolNames(model.calls[0].Tools), "SearchKnowledge")
	assert.NotContains(t, toolNames(model.calls[1].Tools), "SearchKnowledge", "the exhausted tool is removed from the next main Agent request")
	assert.Equal(t, maxSearchKnowledgeCallsPerTurn, eng.searchKnowledgeQueriesThisTurn)

	extra := eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "再检索一次"}, noopStep)
	assert.Contains(t, extra, `"search_limit_reached":true`)
	assert.Len(t, retriever.calls, maxSearchKnowledgeCallsPerTurn, "a late tool call does not bypass the same budget")
}
