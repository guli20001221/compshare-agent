package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/websearch"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type scriptedWebSearcher struct {
	results [][]websearch.Result
	err     error
	calls   []string
}

func (s *scriptedWebSearcher) Search(_ context.Context, query string) ([]websearch.Result, error) {
	s.calls = append(s.calls, query)
	if s.err != nil {
		return nil, s.err
	}
	if len(s.results) == 0 {
		return nil, nil
	}
	result := s.results[0]
	s.results = s.results[1:]
	return result, nil
}

func TestWebSearchIsNotOfferedUntilAnAvailableKBSearchHasNoEvidence(t *testing.T) {
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{Enabled: true, Empty: true}}}
	searcher := &scriptedWebSearcher{results: [][]websearch.Result{{{
		Title: "PyTorch previous versions", URL: "https://pytorch.org/get-started/previous-versions/", Snippet: "官方 CUDA 对应表。",
	}}}}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)
	eng.SetWebSearcher(searcher)
	eng.lastUserMsg = "PyTorch CUDA 版本兼容吗？"
	eng.resolvedKnowledgeQuestionThisTurn = eng.lastUserMsg // keep this unit test off the planner path

	require.NotContains(t, centralAgentToolNamesWithWebSearch(false, false, eng.webSearchMayRun()), webSearchAction)
	_ = eng.executeSearchKnowledge(context.Background(), map[string]any{"query": eng.lastUserMsg}, noopStep)
	require.True(t, eng.webSearchMayRun(), "only an available, empty KB result opens the fallback")
	require.Contains(t, centralAgentToolNamesWithWebSearch(false, false, eng.webSearchMayRun()), webSearchAction)

	out := eng.executeTool(context.Background(), toolCall("web", webSearchAction, `{"query":"PyTorch CUDA 版本兼容吗？"}`), noopStep)
	assert.Contains(t, out, "pytorch.org")
	require.Equal(t, []string{"PyTorch CUDA 版本兼容吗？"}, searcher.calls)
	assert.Equal(t, 1, eng.webSearchCallsThisTurn)
	assert.False(t, eng.webSearchMayRun(), "the one external query must withdraw itself")
}

func TestWebSearchRefusesHallucinatedFirstHopAndModelExpandedQuery(t *testing.T) {
	searcher := &scriptedWebSearcher{}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetWebSearcher(searcher)
	eng.lastUserMsg = "CUDA 不兼容怎么办？"

	firstHop := eng.executeTool(context.Background(), toolCall("web", webSearchAction, `{"query":"CUDA 不兼容怎么办？"}`), noopStep)
	assert.Contains(t, firstHop, "not_available")
	assert.Empty(t, searcher.calls)

	eng.webSearchAvailableThisTurn = true // emulate the only allowed predecessor
	expanded := eng.executeTool(context.Background(), toolCall("web", webSearchAction, `{"query":"Compshare 上 PyTorch CUDA 不兼容怎么办？"}`), noopStep)
	assert.Contains(t, expanded, "invalid_request")
	assert.Empty(t, searcher.calls, "the model may not add platform/account context to an external query")
}

func TestWebSearchRefusesSensitiveUserLiterals(t *testing.T) {
	credential := "ghp_" + strings.Repeat("x", 20)
	for _, query := range []string{
		"我的手机号 13800138000，CUDA 不兼容怎么办？",
		"我的凭据是 " + credential + "，CUDA 不兼容怎么办？",
	} {
		t.Run(query, func(t *testing.T) {
			searcher := &scriptedWebSearcher{}
			eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
			eng.SetWebSearcher(searcher)
			eng.lastUserMsg = query
			eng.webSearchAvailableThisTurn = true

			out := eng.executeSearchWeb(context.Background(), map[string]any{"query": query}, noopStep)
			assert.Contains(t, out, "invalid_request")
			assert.Empty(t, searcher.calls, "sensitive literals must not leave the process")
		})
	}
}

func TestWebSearchDoesNotOpenOnKBOutageAndEvidenceWithdrawsIt(t *testing.T) {
	searcher := &scriptedWebSearcher{}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetWebSearcher(searcher)
	eng.SetKnowledgeRetriever(&scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{
		{Enabled: true, Empty: true, Unavailable: true, FailureReason: "mcp_timeout"},
		{Enabled: true, HitItems: []knowledge.RetrievalHit{{Kept: true, Score: 90, Chunk: knowledge.KBChunk{
			ChunkID: "docs-1", Title: "平台文档", Content: "有可靠答案。",
		}}}},
	}})
	eng.resolvedKnowledgeQuestionThisTurn = "问题"

	_ = eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "问题"}, noopStep)
	assert.False(t, eng.webSearchMayRun(), "an outage is not evidence that a web result is appropriate")

	// A second, successful KB search supplies evidence and must also leave the
	// external fallback closed.
	_ = eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "问题"}, noopStep)
	assert.False(t, eng.webSearchMayRun())
}

func TestAgentLoopRevealsWebSearchOnlyAfterEmptyKBResult(t *testing.T) {
	question := "PyTorch CUDA 版本兼容吗？"
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("kb", "SearchKnowledge", `{"query":"PyTorch CUDA 版本兼容吗？"}`)}},
		{Content: `{"answer_question":"PyTorch CUDA 版本兼容吗？","search_queries":["PyTorch CUDA 版本兼容吗？"]}`},
		{ToolCalls: []openai.ToolCall{toolCall("web", webSearchAction, `{"query":"PyTorch CUDA 版本兼容吗？"}`)}},
		{Content: "可参考 [PyTorch previous versions](https://pytorch.org/get-started/previous-versions/) 的 CUDA 对应表。"},
	}}
	eng := NewWithDeps(model, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(&scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{Enabled: true, Empty: true}}})
	eng.SetWebSearcher(&scriptedWebSearcher{results: [][]websearch.Result{{{
		Title: "PyTorch previous versions", URL: "https://pytorch.org/get-started/previous-versions/", Snippet: "官方 CUDA 对应表。",
	}}}})

	reply, err := eng.Chat(context.Background(), question, noopStep)
	require.NoError(t, err)
	assert.Contains(t, reply, "https://pytorch.org/get-started/previous-versions/")

	var agentCalls []llm.ChatRequest
	for _, call := range model.calls {
		if len(call.Tools) > 0 {
			agentCalls = append(agentCalls, call)
		}
	}
	require.Len(t, agentCalls, 3)
	assert.NotContains(t, toolNames(agentCalls[0].Tools), webSearchAction)
	assert.Contains(t, toolNames(agentCalls[1].Tools), webSearchAction)
	assert.NotContains(t, toolNames(agentCalls[2].Tools), webSearchAction)
}

func TestWebSearchStaysHardDisabledForKnowledgeOnlyTurns(t *testing.T) {
	question := "PyTorch CUDA 版本兼容吗？"
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("kb", "SearchKnowledge", `{"query":"PyTorch CUDA 版本兼容吗？"}`)}},
		{Content: `{"answer_question":"PyTorch CUDA 版本兼容吗？","search_queries":["PyTorch CUDA 版本兼容吗？"]}`},
		// The model cannot see SearchWeb in this window, but an invented call
		// must be rejected at execution as well as hidden from the schema.
		{ToolCalls: []openai.ToolCall{toolCall("web", webSearchAction, `{"query":"PyTorch CUDA 版本兼容吗？"}`)}},
		{Content: "知识库没有足够证据。"},
	}}
	searcher := &scriptedWebSearcher{results: [][]websearch.Result{{{
		Title: "must not be called", URL: "https://example.test/", Snippet: "must not be called",
	}}}}
	eng := NewWithDeps(model, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(&scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{Enabled: true, Empty: true}}})
	eng.SetWebSearcher(searcher)

	_, err := eng.ChatWithOptions(context.Background(), question, noopStep, ChatOptions{KnowledgeOnly: true})
	require.NoError(t, err)
	assert.Empty(t, searcher.calls, "restricted turns must never send a query to an external provider")
	for _, call := range model.calls {
		assert.NotContains(t, toolNames(call.Tools), webSearchAction)
	}
}

func TestWebSearchUnavailableIsVisibleToTheAgent(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.lastUserMsg = "CUDA 不兼容怎么办？"
	eng.webSearchAvailableThisTurn = true
	eng.SetWebSearcher(&scriptedWebSearcher{err: errors.New("upstream unavailable")})

	out := eng.executeTool(context.Background(), toolCall("web", webSearchAction, `{"query":"CUDA 不兼容怎么办？"}`), noopStep)
	assert.Contains(t, out, `"status":"unavailable"`)
}
