package engine

import (
	"context"
	"testing"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExecuteSearchKnowledge_LocalDispatchSubstantive proves the P3 hinge: the
// SearchKnowledge ReAct tool dispatches LOCALLY on the engine retriever (never
// through the external/safe executor — its Route is knowledge, not external_api),
// returns a SUBSTANTIVE EvidenceLedger (a chunk-content snippet the agent can
// ground an actionable answer on, not the content-free diagnosis ledger), and
// records the hits so the final-answer no-raw-leak guard can validate the
// synthesis turn.
func TestExecuteSearchKnowledge_LocalDispatchSubstantive(t *testing.T) {
	content := "降低 vLLM 显存占用：缩短上下文 --max-model-len；降低并发 --max-num-seqs；" +
		"多卡张量并行 --tensor-parallel-size；使用量化 quantization。"
	retriever := &scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{{
		Enabled: true,
		HitItems: []knowledge.RetrievalHit{{
			Kept:  true,
			Score: 90,
			Chunk: knowledge.KBChunk{
				ChunkID:    "ext-gpu-oom-vllm-001",
				Title:      "vLLM 显存不足 (OOM) 排查",
				SourceType: "external",
				Content:    content,
			},
		}},
	}}}
	exec := &mockExecutor{}
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}, exec, nil)
	eng.SetKnowledgeRetriever(retriever)

	tc := openai.ToolCall{
		ID:   "call-sk",
		Type: openai.ToolTypeFunction,
		Function: openai.FunctionCall{
			Name:      "SearchKnowledge",
			Arguments: `{"query":"vllm 显存不足"}`,
		},
	}
	out := eng.executeTool(context.Background(), tc, noopStep)

	// Substantive evidence: chunk id + a real actionable token in the snippet.
	assert.Contains(t, out, "EvidenceLedger")
	assert.Contains(t, out, "ext-gpu-oom-vllm-001")
	assert.Contains(t, out, "--max-model-len", "result must carry actionable content for the agent to ground on")

	// Local dispatch: the external/safe tool executor was NEVER called.
	assert.Empty(t, exec.calls, "SearchKnowledge must dispatch locally, never via the API/safe executor")

	// Retriever ran with the query; hits recorded for the synthesis guard.
	require.Len(t, retriever.calls, 1)
	assert.Equal(t, "vllm 显存不足", retriever.calls[0].question)
	assert.True(t, eng.searchKnowledgeRanThisTurn)
	assert.Len(t, eng.searchKnowledgeHitsThisTurn, 1)
}
