package httpapi

import (
	"context"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type ragTracePlanner struct{}

func (ragTracePlanner) Plan(context.Context, intent.IntentRouterInput) (intent.IntentRouterResult, error) {
	return intent.IntentRouterResult{Plan: intent.IntentRoute{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentKnowledgeQA,
		Slots:         intent.Slots{},
		Confidence:    0.9,
	}}, nil
}

type ragTraceRetriever struct {
	chunk knowledge.KBChunk
}

func (r ragTraceRetriever) Retrieve(question, productArea string) knowledge.RetrievalResult {
	return knowledge.RetrievalResult{
		Enabled:   true,
		KBVersion: "kb.v1",
		Hits:      []knowledge.KBChunk{r.chunk},
		HitItems:  []knowledge.RetrievalHit{{Chunk: r.chunk, Score: 90, Kept: true}},
	}
}

type ragTraceLLM struct {
	responses []llm.ChatResponse
}

func (m *ragTraceLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	if len(m.responses) == 0 {
		return &llm.ChatResponse{Content: ""}, nil
	}
	resp := m.responses[0]
	m.responses = m.responses[1:]
	if req.OnTextDelta != nil && resp.Content != "" {
		req.OnTextDelta(resp.Content)
	}
	return &resp, nil
}

func TestDispatchChat_KnowledgeAgentLoopWritesCitationTrace(t *testing.T) {
	engine.SetKnowledgeQAAgentLoopEnabled(true)
	defer engine.SetKnowledgeQAAgentLoopEnabled(false)
	engine.SetDisciplinedKnowledgeQASynthesisEnabled(false)
	defer engine.SetDisciplinedKnowledgeQASynthesisEnabled(false)
	engine.SetGroundedAnswerValidatorEnabled(false)
	tools.SetAgenticSearchKnowledgeEnabled(true)
	defer tools.SetAgenticSearchKnowledgeEnabled(false)

	chunk := knowledge.KBChunk{
		ChunkID:    "faq-billing-001",
		KBVersion:  "kb.v1",
		SourceType: "runbook",
		Title:      "Billing after stop",
		Content:    "Stopped on-demand instances still charge for disks.",
		SourceURL:  "https://example.test/billing",
	}
	llmClient := &ragTraceLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{{
			ID:       "call-sk",
			Type:     openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: "SearchKnowledge", Arguments: `{"query":"why do stopped instances still bill"}`},
		}}},
		{
			Content: "停止的按量实例的磁盘仍会计费 [[faq-billing-001]]。",
			Usage:   llm.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		},
	}}
	eng := engine.NewWithDeps(llmClient, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)
	eng.SetIntentPlanner(ragTracePlanner{}, engine.IntentPlannerOptions{Model: "deepseek-v4-flash"})
	eng.SetKnowledgeRetriever(ragTraceRetriever{chunk: chunk})

	traceWriter := &captureTraceWriter{}
	messages := &recordingMessages{}
	h := NewHandlers(
		&config.Config{Agent: config.AgentConfig{
			LLM:  config.LLMConfig{Model: "model-x"},
			HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: time.Hour},
			Meta: config.MetaConfig{MaxInputLength: 4000},
			STS:  config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
		}},
		&mockSessions{byID: map[string]store.Session{
			"sess-rag": {
				ID:                "sess-rag",
				TopOrganizationID: 7,
				OrganizationID:    8,
				CreatedAt:         time.Now(),
				UpdatedAt:         time.Now(),
			},
		}},
		messages,
		mockFeedback{},
		fakePool{eng: eng},
		traceWriter,
	)

	sink, apiErr := runChatJSON(t, h, `{"Action":"SendCSAgentChat","SessionId":"sess-rag","Message":"why do stopped instances still bill","request_uuid":"req-rag","top_organization_id":7,"organization_id":8}`)
	require.Nil(t, apiErr)
	assert.True(t, sink.has("done"))
	assert.NotContains(t, sink.body(), "[[faq-billing-001]]")
	require.Len(t, traceWriter.records, 1)

	retrieval := traceWriter.records[0].Retrieval
	assert.Equal(t, 1, retrieval.Hits)
	assert.Equal(t, []string{"faq-billing-001"}, retrieval.CitedChunkIDs)
	assert.Equal(t, []string{"1"}, retrieval.CitedRefs)
	require.Len(t, retrieval.References, 1)
	assert.Equal(t, "1", retrieval.References[0].RefID)
	assert.Equal(t, "faq-billing-001", retrieval.References[0].ChunkID)
	assert.Equal(t, "Billing after stop", retrieval.References[0].Title)
}
