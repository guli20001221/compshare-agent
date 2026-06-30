package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFlashKnowledgeRouteGuardDefaultOff(t *testing.T) {
	SetFlashKnowledgeRouteGuardEnabled(false)
	t.Cleanup(func() { SetFlashKnowledgeRouteGuardEnabled(false) })

	require.False(t, FlashKnowledgeRouteGuardEnabled())
}

func TestFlashKnowledgeRouteGuardMatch(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{name: "disk billing", text: "磁盘空间是如何收费的？100GB 原始空间免费吗", want: true},
		{name: "coding plan cancel refund", text: "取消 Coding Plan 套餐能退款吗", want: true},
		{name: "coding plan delete paraphrase", text: "把 Coding Plan 套餐退了", want: true},
		{name: "generic shortage", text: "一直暂无资源 是什么情况", want: true},
		{name: "sold out semantics", text: "SoldOut 是售罄还是下架", want: true},
		{name: "normal semantics", text: "Normal 状态是不是说明一定有库存", want: true},
		{name: "named gpu stock stays live", text: "5090一直暂无资源是什么情况", want: false},
		{name: "named gpu availability stays live", text: "4090 有没有货", want: false},
		{name: "instance lifecycle stays workflow", text: "帮我重启这台实例", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := matchFlashKnowledgeRouteGuard(tt.text)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestFlashKnowledgeRouteGuardUsesAgenticRAGWhenAvailable(t *testing.T) {
	SetFlashKnowledgeRouteGuardEnabled(true)
	t.Cleanup(func() { SetFlashKnowledgeRouteGuardEnabled(false) })
	SetKnowledgeQAAgentLoopEnabled(true)
	t.Cleanup(func() { SetKnowledgeQAAgentLoopEnabled(false) })
	tools.SetAgenticSearchKnowledgeEnabled(true)
	t.Cleanup(func() { tools.SetAgenticSearchKnowledgeEnabled(false) })

	chunk := knowledge.KBChunk{
		ChunkID:     "w0-billing_rule-disk",
		KBVersion:   "kb.v1",
		SourceType:  "faq",
		ProductArea: "billing_rule",
		Title:       "磁盘计费",
		Content:     "系统盘默认容量免费，数据盘创建后计费。",
	}
	planner := &scriptedIntentPlanner{results: []intent.IntentRouterResult{{Plan: intent.IntentRoute{
		SchemaVersion: intent.SchemaVersion,
		Intent:        intent.IntentResourceInfo,
		Confidence:    0.9,
	}}}}
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled:    true,
		KBVersion:  "kb.v1",
		Hits:       []knowledge.KBChunk{chunk},
		HitItems:   []knowledge.RetrievalHit{{Chunk: chunk, Score: 0.91, Kept: true}},
		HybridMode: "qwen3_rrf",
	}}}
	mock := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{{ID: "sk", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "SearchKnowledge", Arguments: `{"query":"磁盘空间如何收费"}`}}}},
		{Content: "数据盘创建后会计费 [[w0-billing_rule-disk]]。"},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.SetIntentPlanner(planner, IntentPlannerOptions{Model: "deepseek-v4-flash"})
	eng.SetKnowledgeRetriever(retriever)
	var traces []observability.RetrievalTrace
	eng.SetRetrievalTraceObserver(func(trace observability.RetrievalTrace) {
		traces = append(traces, trace)
	})

	reply, err := eng.Chat(context.Background(), "磁盘空间是如何收费的？100GB 原始空间免费吗", noopStep)

	require.NoError(t, err)
	assert.True(t, eng.knowledgeQAAgentLoopThisTurn)
	assert.Contains(t, reply, "数据盘创建后会计费")
	require.NotEmpty(t, traces)
	assert.NotNil(t, traces[0].QueryPlan)
	assert.NotEmpty(t, traces[0].Activities)
	assert.NotEmpty(t, traces[0].References)
}
