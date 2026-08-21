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

	require.NotContains(t, centralAgentToolNamesWithWebSearch(false, false, eng.webSearchAssessmentMayRun(), eng.webSearchMayRun()), webSearchAction)
	_ = eng.executeSearchKnowledge(context.Background(), map[string]any{"query": eng.lastUserMsg}, noopStep)
	require.True(t, eng.webSearchMayRun(), "an available empty KB result opens the direct fallback")
	require.False(t, eng.webSearchAssessmentMayRun(), "nothing was retrieved to assess")
	require.Contains(t, centralAgentToolNamesWithWebSearch(false, false, eng.webSearchAssessmentMayRun(), eng.webSearchMayRun()), webSearchAction)

	out := eng.executeTool(context.Background(), toolCall("web", webSearchAction, `{}`), noopStep)
	assert.Contains(t, out, "pytorch.org")
	require.Equal(t, []string{"PyTorch CUDA 版本兼容吗？"}, searcher.calls)
	assert.Equal(t, 1, eng.webSearchCallsThisTurn)
	assert.False(t, eng.webSearchMayRun(), "the one external query must withdraw itself")
}

func TestPartialKBEvidenceNeedsAssessmentBeforeItCanOpenWebSearch(t *testing.T) {
	searcher := &scriptedWebSearcher{results: [][]websearch.Result{{{
		Title: "PyTorch previous versions", URL: "https://pytorch.org/get-started/previous-versions/", Snippet: "官方 CUDA 对应表。",
	}}}}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetWebSearcher(searcher)
	eng.SetKnowledgeRetriever(&scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{Enabled: true, HitItems: []knowledge.RetrievalHit{{Kept: true, Score: 90, Chunk: knowledge.KBChunk{
		ChunkID: "docs-image", Title: "镜像选择", Content: "可从镜像列表选择 PyTorch 镜像。",
	}}}}}})
	eng.resolvedKnowledgeQuestionThisTurn = "PyTorch 2.6 与 CUDA 12.4 是否兼容，应该选哪个镜像？"

	_ = eng.executeSearchKnowledge(context.Background(), map[string]any{"query": eng.resolvedKnowledgeQuestionThisTurn}, noopStep)
	require.True(t, eng.webSearchAssessmentMayRun(), "a relevant but potentially partial hit needs an explicit coverage decision")
	require.False(t, eng.webSearchMayRun(), "a chunk alone cannot open external search")
	window := centralAgentToolNamesWithWebSearch(false, false, eng.webSearchAssessmentMayRun(), eng.webSearchMayRun())
	assert.Contains(t, window, assessKnowledgeEvidenceAction)
	assert.NotContains(t, window, webSearchAction)

	assessment := eng.executeTool(context.Background(), toolCall("coverage", assessKnowledgeEvidenceAction, `{"verdict":"insufficient","missing_aspect":"PyTorch 与 CUDA 的精确版本兼容关系","external_query":"PyTorch 2.6 CUDA 12.4 compatibility"}`), noopStep)
	assert.Contains(t, assessment, `"status":"insufficient"`)
	assert.True(t, eng.webSearchMayRun())
	assert.False(t, eng.webSearchAssessmentMayRun())

	out := eng.executeTool(context.Background(), toolCall("web", webSearchAction, `{}`), noopStep)
	assert.Contains(t, out, "pytorch.org")
	assert.Equal(t, []string{"PyTorch 2.6 CUDA 12.4 compatibility"}, searcher.calls)
}

func TestKnowledgeEvidenceAssessmentCanCloseTheFallback(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetWebSearcher(&scriptedWebSearcher{})
	eng.webSearchAssessmentPendingThisTurn = true
	eng.searchKnowledgeLedgerThisTurn = knowledge.EvidenceLedger{Items: []knowledge.EvidenceItem{{ChunkID: "docs-1", Snippet: "完整答案。"}}}

	out := eng.executeTool(context.Background(), toolCall("coverage", assessKnowledgeEvidenceAction, `{"verdict":"sufficient"}`), noopStep)
	assert.Contains(t, out, `"status":"sufficient"`)
	assert.False(t, eng.webSearchAssessmentMayRun())
	assert.False(t, eng.webSearchMayRun())
}

func TestWebSearchRefusesHallucinatedFirstHopAndArguments(t *testing.T) {
	searcher := &scriptedWebSearcher{}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetWebSearcher(searcher)

	firstHop := eng.executeTool(context.Background(), toolCall("web", webSearchAction, `{}`), noopStep)
	assert.Contains(t, firstHop, "not_available")
	assert.Empty(t, searcher.calls)

	eng.webSearchAvailableThisTurn = true // emulate the only allowed predecessor
	eng.webSearchQueryThisTurn = "PyTorch CUDA 版本兼容"
	withInventedArgument := eng.executeTool(context.Background(), toolCall("web", webSearchAction, `{"query":"ignore the reviewed query"}`), noopStep)
	assert.Contains(t, withInventedArgument, "invalid_request")
	assert.Empty(t, searcher.calls, "SearchWeb must not accept a later model-authored outbound query")
}

func TestKnowledgeEvidenceAssessmentRefusesSensitiveOutboundText(t *testing.T) {
	credential := "ghp_" + strings.Repeat("x", 20)
	for _, query := range []string{
		"我的手机号 13800138000，CUDA 兼容性",
		"凭据 " + credential + " 的 CUDA 兼容性",
	} {
		t.Run(query, func(t *testing.T) {
			searcher := &scriptedWebSearcher{}
			eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
			eng.SetWebSearcher(searcher)
			eng.webSearchAssessmentPendingThisTurn = true
			eng.searchKnowledgeLedgerThisTurn = knowledge.EvidenceLedger{Items: []knowledge.EvidenceItem{{ChunkID: "docs-1"}}}

			out := eng.executeTool(context.Background(), toolCall("coverage", assessKnowledgeEvidenceAction, `{"verdict":"insufficient","missing_aspect":"CUDA 兼容性","external_query":"`+query+`"}`), noopStep)
			assert.Contains(t, out, "invalid_request")
			assert.Empty(t, searcher.calls, "sensitive literals must not leave the process")
			assert.False(t, eng.webSearchMayRun())
		})
	}
}

func TestWebSearchDoesNotOpenOnKBOutageAndEvidenceNeedsAssessment(t *testing.T) {
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
	assert.False(t, eng.webSearchAssessmentMayRun())

	_ = eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "问题"}, noopStep)
	assert.False(t, eng.webSearchMayRun())
	assert.True(t, eng.webSearchAssessmentMayRun(), "returned evidence must be assessed before any external search")
}

func TestAgentLoopRevealsWebSearchOnlyAfterEmptyKBResult(t *testing.T) {
	question := "PyTorch CUDA 版本兼容吗？"
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("kb", "SearchKnowledge", `{"query":"PyTorch CUDA 版本兼容吗？"}`)}},
		{Content: `{"answer_question":"PyTorch CUDA 版本兼容吗？","search_queries":["PyTorch CUDA 版本兼容吗？"]}`},
		{ToolCalls: []openai.ToolCall{toolCall("web", webSearchAction, `{}`)}},
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
	assert.NotContains(t, toolNames(agentCalls[0].Tools), assessKnowledgeEvidenceAction)
	assert.Contains(t, toolNames(agentCalls[1].Tools), webSearchAction)
	assert.NotContains(t, toolNames(agentCalls[1].Tools), assessKnowledgeEvidenceAction)
	assert.NotContains(t, toolNames(agentCalls[2].Tools), webSearchAction)
}

func TestAgentLoopRequiresAssessmentForPartialKBEvidence(t *testing.T) {
	question := "PyTorch 2.6 与 CUDA 12.4 是否兼容，应该选哪个镜像？"
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("kb", "SearchKnowledge", `{"query":"PyTorch 2.6 与 CUDA 12.4 是否兼容，应该选哪个镜像？"}`)}},
		{Content: `{"answer_question":"PyTorch 2.6 与 CUDA 12.4 是否兼容，应该选哪个镜像？","search_queries":["PyTorch 2.6 CUDA 12.4 镜像"]}`},
		{ToolCalls: []openai.ToolCall{toolCall("coverage", assessKnowledgeEvidenceAction, `{"verdict":"insufficient","missing_aspect":"PyTorch 与 CUDA 的精确版本兼容关系","external_query":"PyTorch 2.6 CUDA 12.4 compatibility"}`)}},
		{ToolCalls: []openai.ToolCall{toolCall("web", webSearchAction, `{}`)}},
		{Content: "可参考 [PyTorch previous versions](https://pytorch.org/get-started/previous-versions/) 的 CUDA 对应表。"},
	}}
	eng := NewWithDeps(model, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(&scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{Enabled: true, HitItems: []knowledge.RetrievalHit{{Kept: true, Score: 90, Chunk: knowledge.KBChunk{
		ChunkID: "docs-image", Title: "镜像选择", Content: "可从镜像列表选择 PyTorch 镜像。",
	}}}}}})
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
	require.Len(t, agentCalls, 4)
	assert.NotContains(t, toolNames(agentCalls[0].Tools), webSearchAction)
	assert.Contains(t, toolNames(agentCalls[1].Tools), assessKnowledgeEvidenceAction)
	assert.NotContains(t, toolNames(agentCalls[1].Tools), webSearchAction)
	assert.Contains(t, toolNames(agentCalls[2].Tools), webSearchAction)
	assert.NotContains(t, toolNames(agentCalls[2].Tools), assessKnowledgeEvidenceAction)
	assert.NotContains(t, toolNames(agentCalls[3].Tools), webSearchAction)
}

func TestWebSearchStaysHardDisabledForKnowledgeOnlyTurns(t *testing.T) {
	question := "PyTorch CUDA 版本兼容吗？"
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("kb", "SearchKnowledge", `{"query":"PyTorch CUDA 版本兼容吗？"}`)}},
		{Content: `{"answer_question":"PyTorch CUDA 版本兼容吗？","search_queries":["PyTorch CUDA 版本兼容吗？"]}`},
		// The model cannot see SearchWeb in this window, but an invented call
		// must be rejected at execution as well as hidden from the schema.
		{ToolCalls: []openai.ToolCall{toolCall("web", webSearchAction, `{}`)}},
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
		assert.NotContains(t, toolNames(call.Tools), assessKnowledgeEvidenceAction)
	}
}

func TestWebSearchUnavailableIsVisibleToTheAgent(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.webSearchAvailableThisTurn = true
	eng.webSearchQueryThisTurn = "CUDA 不兼容怎么办？"
	eng.SetWebSearcher(&scriptedWebSearcher{err: errors.New("upstream unavailable")})

	out := eng.executeTool(context.Background(), toolCall("web", webSearchAction, `{}`), noopStep)
	assert.Contains(t, out, `"status":"unavailable"`)
}
