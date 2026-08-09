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

// A knowledge answer must be FINALIZED (its citation markers stripped by the
// deterministic final gate) BEFORE any token reaches the client — not after.
// Under typography-only grounding the finalizer no longer replaces "unsupported"
// prose (the semantic verifier is gone; fail-open ships the Agent's answer), but
// the streaming invariant is unchanged: a searched turn buffers instead of
// streaming live, so the raw [[chunk_id]] marker never appears in the token stream.
func TestKnowledgeAnswerIsFinalizedBeforeAnyTokenReachesTheClient(t *testing.T) {
	const chunkID = "prior-refund-policy"
	chunk := knowledge.KBChunk{ChunkID: chunkID, KBVersion: "kb.v1", Title: "退款规则", Content: "该订单不支持退款。"}
	mock := &streamingSeqMockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("search", "SearchKnowledge", `{"query":"该订单是否支持退款"}`)}},
		plannerEcho("该订单是否支持退款"),
		{Content: "该订单不支持退款[[" + chunkID + "]]。"},
	}}
	eng := NewWithDeps(mock, &mockExecutor{}, nil)
	eng.InitWithContext("test")
	eng.SetKnowledgeRetriever(&scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled: true, KBVersion: "kb.v1", Hits: []knowledge.KBChunk{chunk},
		HitItems: []knowledge.RetrievalHit{{Chunk: chunk, Score: 90, Kept: true}},
	}}})

	var deltas []string
	reply, err := eng.ChatWithOptions(context.Background(), "该订单能退款吗", noopStep, ChatOptions{
		OnTextDelta: func(delta string) { deltas = append(deltas, delta) },
	})
	require.NoError(t, err)
	assert.Equal(t, "该订单不支持退款。", reply, "the citation marker is stripped in the final reply")
	assert.Equal(t, reply, strings.Join(deltas, ""), "the stripped answer is what streamed — markers never hit the wire")
	assert.NotContains(t, strings.Join(deltas, ""), "[[", "the raw marker must never reach the token stream")
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
