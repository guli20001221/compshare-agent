package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// streamingSeqMockLLM returns scripted responses in order AND streams each one's content
// through OnTextDelta, the way the real client does. The plain mockLLM never calls
// OnTextDelta, which is why a bug that only manifests on the token stream can hide behind
// it — and did.
type streamingSeqMockLLM struct {
	responses []llm.ChatResponse
	idx       int
}

func TestKnowledgeFollowupIsVerifiedBeforeAnyTokenReachesTheClient(t *testing.T) {
	const chunkID = "prior-refund-policy"
	chunk := knowledge.KBChunk{ChunkID: chunkID, KBVersion: "kb.v1", Title: "退款规则", Content: "该订单不支持退款。"}
	mock := &streamingSeqMockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("search", "SearchKnowledge", `{"query":"该订单是否支持退款"}`)}},
		{Content: "所有订单都可以全额退款。"},
		{Content: `{"supported":false,"claims":[],"unsupported":["所有订单都可以全额退款"]}`},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test")
	eng.SetKnowledgeRetriever(&scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled: true, KBVersion: "kb.v1", Hits: []knowledge.KBChunk{chunk},
		HitItems: []knowledge.RetrievalHit{{Chunk: chunk, Score: 90, Kept: true}},
	}}})
	eng.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaCurrent,
		VerifiedKnowledge: []VerifiedKnowledgeTurn{{
			Question: "订单是否支持退款",
			Answer:   "该订单不支持退款。",
			Evidence: knowledge.EvidenceLedger{Query: "订单是否支持退款", Items: []knowledge.EvidenceItem{{
				ChunkID: chunkID, Title: "退款规则", Snippet: "该订单不支持退款。",
			}}},
		}},
	}, 0)

	var deltas []string
	reply, err := eng.ChatWithOptions(context.Background(), "那这个呢？", noopStep, ChatOptions{
		OnTextDelta: func(delta string) { deltas = append(deltas, delta) },
	})
	require.NoError(t, err)
	assert.Equal(t, ragUngroundableReply, reply)
	assert.Equal(t, ragUngroundableReply, strings.Join(deltas, ""),
		"unsupported model prose must be replaced before, not after, streaming")
	assert.NotContains(t, strings.Join(deltas, ""), "全额退款")
}

func (m *streamingSeqMockLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	if m.idx >= len(m.responses) {
		return &llm.ChatResponse{Content: "no more mock responses"}, nil
	}
	resp := m.responses[m.idx]
	m.idx++
	if req.OnTextDelta != nil && resp.Content != "" {
		req.OnTextDelta(resp.Content)
	}
	return &resp, nil
}

// The bug this pins shipped past my own live smoke test, because the replay harness reads
// the FINAL reply and the final reply was correct. Meanwhile the browser would have been
// fed the literal characters "{{INSTANCE_TABLE}}" as they were generated, and only then
// been handed a different final frame.
//
// Substituting on the way OUT of ChatWithOptions is too late: by then the tokens are gone.
// The rewrite has to be declared in guardMayRewrite (so the round buffers instead of
// streaming live) and applied to `content` inside the loop, which is the contract every
// other post-hoc rewrite in this engine already follows.
func TestPlaceholderNeverReachesTheTokenStream(t *testing.T) {
	SetAgentDeterministicRenderEnabled(true)
	defer SetAgentDeterministicRenderEnabled(false)

	mock := &streamingSeqMockLLM{responses: []llm.ChatResponse{
		// Round 1: the model looks the instances up.
		{ToolCalls: []openai.ToolCall{
			toolCall("tc1", "DescribeCompShareInstance", `{}`),
		}},
		// Round 2: it writes prose and defers the list to the placeholder, as instructed.
		{Content: "好的，以下是您的实例：\n\n" + instanceTablePlaceholder + "\n\n需要关机请告诉我。"},
	}}
	exec := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {
			"TotalCount": 1.0,
			"UHostSet": []any{
				map[string]any{
					"UHostId": "uhost-realone01", "Name": "host", "State": "Running",
					"Zone": "cn-wlcb-01", "GpuType": "4090", "GPU": 1.0, "CPU": 16.0, "Memory": 64.0,
				},
			},
		},
	}}

	eng := NewWithDeps(mock, exec, nil)
	eng.InitWithContext("test user")

	var deltas []string
	reply, err := eng.ChatWithOptions(context.Background(), "我目前部署的实例", noopStep, ChatOptions{
		OnTextDelta: func(d string) { deltas = append(deltas, d) },
	})
	require.NoError(t, err)

	streamed := strings.Join(deltas, "")

	// The user must never SEE the placeholder. This is the assertion that fails if the
	// substitution is done on the returned value instead of inside the loop.
	assert.NotContains(t, streamed, instanceTablePlaceholder,
		"the raw placeholder was streamed to the client as tokens")
	assert.NotContains(t, reply, instanceTablePlaceholder,
		"the placeholder survived into the final reply")

	// And what they DO see is our table, in both the stream and the final frame — the two
	// must agree, or the client renders one thing and persists another.
	assert.Contains(t, streamed, "uhost-realone01",
		"the rendered instance table never reached the token stream")
	assert.Contains(t, reply, "uhost-realone01")
	assert.Contains(t, reply, "需要关机请告诉我", "the model's own prose was destroyed")
}
