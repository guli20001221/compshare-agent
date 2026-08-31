package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type directAnswerReviewFailureLLM struct {
	calls     int
	second    llm.ChatResponse
	secondErr error
}

func (m *directAnswerReviewFailureLLM) Chat(_ context.Context, _ llm.ChatRequest) (*llm.ChatResponse, error) {
	m.calls++
	if m.calls == 1 {
		return &llm.ChatResponse{Content: "第一轮已经完成的答复。"}, nil
	}
	if m.secondErr != nil {
		return nil, m.secondErr
	}
	return &m.second, nil
}

func TestDirectAnswerReviewLetsTheCentralAgentChooseKnowledge(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{
		{Content: "未查资料就直接回答。"},
		{ToolCalls: []openai.ToolCall{toolCall("search", "SearchKnowledge", `{"query":"无卡模式启动是什么意思"}`)}},
		{Content: `{"answer_question":"无卡模式启动是什么意思","search_queries":["无卡模式启动规则"]}`},
		{Content: "无卡模式启动时不挂载 GPU。[[chunk-no-gpu]]"},
	}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled: true,
		HitItems: []knowledge.RetrievalHit{{
			Kept:  true,
			Score: 0.94,
			Chunk: knowledge.KBChunk{
				ChunkID: "chunk-no-gpu",
				Title:   "无卡模式",
				Content: "无卡模式启动时不挂载 GPU，原 GPU 配置会保留。",
			},
		}},
	}}}
	eng := NewWithDeps(model, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "无卡模式启动是什么意思？", noopStep)
	require.NoError(t, err)
	assert.Contains(t, reply, "不挂载 GPU")
	assert.NotContains(t, reply, "未查资料")
	require.Len(t, retriever.calls, 2, "the existing planner keeps its written query plus the user's original wording")
	require.Len(t, model.calls, 4, "review, tool selection, query plan, and grounded answer stay in one Agent loop")
	assert.Contains(t, renderTestMessages(model.calls[1].Messages), directAnswerToolReviewInstruction)
	assert.Contains(t, toolNames(model.calls[1].Tools), "SearchKnowledge")
}

func TestDirectAnswerReviewAcceptsTheAgentsSecondDirectDecision(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{
		{Content: "你好。"},
		{Content: "你好！有什么可以帮你？"},
	}}
	retriever := &scriptedKnowledgeRetriever{}
	eng := NewWithDeps(model, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(retriever)

	reply, err := eng.Chat(context.Background(), "你好", noopStep)
	require.NoError(t, err)
	assert.Equal(t, "你好！有什么可以帮你？", reply)
	assert.Empty(t, retriever.calls, "the review never calls SearchKnowledge on the Agent's behalf")
	require.Len(t, model.calls, 2)
	assert.Contains(t, renderTestMessages(model.calls[1].Messages), directAnswerToolReviewInstruction)
}

func TestDirectAnswerReviewDoesNotAddARoundAfterALiveRead(t *testing.T) {
	readTool := capability.ReadToolName(intent.IntentResourceInfo)
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("instances", readTool, `{}`)}},
		{Content: "当前账号有一台运行中的实例。"},
	}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"TotalCount": float64(1),
			"UHostSet": []any{map[string]any{
				"UHostId": "uhost-1", "Name": "train-a", "State": "Running",
			}},
		},
	}}
	eng := NewWithDeps(model, executor, nil)
	eng.SetKnowledgeRetriever(&scriptedKnowledgeRetriever{})

	reply, err := eng.Chat(context.Background(), "我有哪些实例？", noopStep)
	require.NoError(t, err)
	assert.Contains(t, reply, "运行中")
	require.Len(t, model.calls, 2, "a live-tool turn must not enter the direct-answer review path")
	assert.NotContains(t, renderTestMessages(model.calls[1].Messages), directAnswerToolReviewInstruction)
}

func TestDirectAnswerWithoutKnowledgeRetrieverKeepsOneModelCall(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{{Content: "直接回答。"}}}
	eng := NewWithDeps(model, &mockExecutor{}, nil)

	reply, err := eng.Chat(context.Background(), "普通问题", noopStep)
	require.NoError(t, err)
	assert.Equal(t, "直接回答。", reply)
	require.Len(t, model.calls, 1)
}

func TestDirectAnswerReviewFailureKeepsTheCompletedFirstDraft(t *testing.T) {
	cases := []struct {
		name     string
		response llm.ChatResponse
		err      error
	}{
		{name: "model error", err: errors.New("temporary model failure")},
		{name: "empty response", response: llm.ChatResponse{}},
		{name: "truncated response", response: llm.ChatResponse{Content: "不完整", StopReason: "length"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := &directAnswerReviewFailureLLM{second: tc.response, secondErr: tc.err}
			eng := NewWithDeps(model, &mockExecutor{}, nil)
			eng.SetKnowledgeRetriever(&scriptedKnowledgeRetriever{})

			reply, err := eng.Chat(context.Background(), "普通问题", noopStep)
			require.NoError(t, err)
			assert.Equal(t, "第一轮已经完成的答复。", reply)
			assert.Equal(t, 2, model.calls)
		})
	}
}

func TestDirectAnswerReviewRateLimitKeepsTheCompletedFirstDraft(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{{Content: "第一轮已经完成的答复。"}}}
	eng := NewWithDeps(model, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(&scriptedKnowledgeRetriever{})
	eng.rateLimiter = &scriptedRateLimiter{decisions: []governance.Decision{
		{Allowed: true},
		{Allowed: false, Reason: "test limit"},
	}}

	reply, err := eng.Chat(context.Background(), "普通问题", noopStep)
	require.NoError(t, err)
	assert.Equal(t, "第一轮已经完成的答复。", reply)
	require.Len(t, model.calls, 1, "the second model call is blocked before dispatch")
}
