package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/knowledge"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
)

// TestRetrievalTraceReportsNoFloorOnRerankerFallback is the end-to-end half of
// TestFloorValueReportsOnlyAFloorThatActuallyRan: it drives the real
// executeSearchKnowledge path and reads what an operator would actually see.
//
// The scenario is already supported and already correct on the ledger side
// (TestExecuteSearchKnowledge_RerankerFallbackKeepsHits) — qwen3_rrf keeps its
// label through a reranker fallback while its scores revert to the RRF fusion
// scale. The trace was the half that lied: it printed floor_value=0.5 beside a
// 0.031 score for a comparison that never happened, which reads as "this
// evidence was measured and barely survived" instead of "the reranker fell
// back".
func TestRetrievalTraceReportsNoFloorOnRerankerFallback(t *testing.T) {
	fusionHit := knowledge.RetrievalHit{
		Kept:  true,
		Score: 0.031, // RRF fusion scale, NOT a reranker relevance score
		Chunk: knowledge.KBChunk{
			ChunkID:    "w0-vllm-oom",
			Title:      "vLLM 显存不足",
			SourceType: "external",
			Content:    "降低 max_model_len 或 gpu_memory_utilization。",
		},
	}

	t.Run("reranker fell back: no floor ran, so none is reported", func(t *testing.T) {
		trace := traceForSearch(t, knowledge.RetrievalResult{
			Enabled:                true,
			HybridMode:             knowledge.RetrievalModeQwen3RRF,
			RerankerMode:           "", // the fallback signal
			RerankerFallbackReason: "reranker_timeout",
			HitItems:               []knowledge.RetrievalHit{fusionHit},
		})

		require.Equal(t, knowledge.RetrievalModeQwen3RRF, trace.HybridMode,
			"premise: the mode KEEPS its label through the fallback, which is what made this lie plausible")
		require.Equal(t, "reranker_timeout", trace.RerankerFallbackReason,
			"premise: the real cause is recorded, and floor_value must not compete with it")
		require.False(t, trace.FloorDroppedAll, "premise: the hit survived, so no floor rejected it")

		assert.Zero(t, trace.FloorValue,
			"no comparison happened; reporting 0.5 next to a 0.031 score sends an operator to "+
				"look at scores when the fault is the reranker fallback")
	})

	t.Run("control: the reranker scored, so the floor is reported", func(t *testing.T) {
		scored := fusionHit
		scored.Score = 0.72 // a real qwen3-reranker relevance score
		trace := traceForSearch(t, knowledge.RetrievalResult{
			Enabled:      true,
			HybridMode:   knowledge.RetrievalModeQwen3RRF,
			RerankerMode: "qwen3-reranker-8b",
			HitItems:     []knowledge.RetrievalHit{scored},
		})

		assert.Equal(t, weakEvidenceSemanticThreshold, trace.FloorValue,
			"a floor that ran must still be reportable, or this fix would just be deleting the field")
	})

	t.Run("an unavailable remote compared nothing", func(t *testing.T) {
		trace := traceForSearch(t, knowledge.RetrievalResult{
			Enabled:       true,
			Unavailable:   true,
			Empty:         true,
			FailureReason: "mcp_timeout",
		})

		require.True(t, trace.Unavailable, "premise: this is the operational-failure path")
		assert.Zero(t, trace.FloorValue,
			"a retrieval that never returned hits cannot have applied a floor to them")
	})
}

// traceForSearch runs one SearchKnowledge against a scripted retrieval result
// and returns the per-query RetrievalTrace the engine emitted for it.
func traceForSearch(t *testing.T, result knowledge.RetrievalResult) observability.RetrievalTrace {
	t.Helper()
	eng := NewWithDeps(&mockLLM{responses: []llm.ChatResponse{{Content: "ok"}}}, &mockExecutor{}, nil)
	eng.SetKnowledgeRetriever(&scriptedKnowledgeRetriever{results: []knowledge.RetrievalResult{result}})
	eng.knowledgeQAAgentLoopThisTurn = true

	var traces []observability.RetrievalTrace
	eng.SetRetrievalTraceObserver(func(trace observability.RetrievalTrace) { traces = append(traces, trace) })
	_ = eng.executeSearchKnowledge(context.Background(), map[string]any{"query": "vllm 显存不足"}, noopStep)

	require.NotEmpty(t, traces, "the search must emit a retrieval trace")
	return traces[0]
}
