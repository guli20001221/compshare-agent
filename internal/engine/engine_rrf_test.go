package engine

import (
	"testing"
	"time"

	"github.com/compshare-agent/internal/envelope"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProjectEvidenceTraceHitsPropagatesRRFFields verifies that the
// engine's trace projection function copies the RRF-only fields
// (BM25Rank, DenseRank, FusionRank, FusionScore) from knowledge.RetrievalHit
// onto observability.RetrievalHit. Without this wiring the new trace
// fields would be silently dropped when the retriever produced them.
func TestProjectEvidenceTraceHitsPropagatesRRFFields(t *testing.T) {
	producedAt := time.Now().UTC()
	score := 0.42
	ev, err := envelope.NewEvidence(envelope.EvidenceInput{
		SourceTitle:     "rrf-test-source",
		Snippet:         "rrf trace projection snippet",
		EvidenceKind:    envelope.EvidenceKindKnowledge,
		ChunkID:         "rrf-chunk-a",
		KBVersion:       "kb-test",
		RetrievalScore:  &score,
		QueryNormalized: "test query",
		ProducedAt:      producedAt,
	})
	require.NoError(t, err)

	items := []knowledge.RetrievalHit{
		{
			Chunk:       knowledge.KBChunk{ChunkID: "rrf-chunk-a"},
			Score:       0.42,
			Kept:        true,
			BM25Rank:    2,
			DenseRank:   5,
			FusionRank:  1,
			FusionScore: 0.0301,
		},
	}

	hits := projectEvidenceTraceHits([]envelope.Evidence{ev}, items)
	require.Len(t, hits, 1)
	assert.Equal(t, "rrf-chunk-a", hits[0].ChunkID)
	assert.True(t, hits[0].Kept)
	assert.Equal(t, 2, hits[0].BM25Rank)
	assert.Equal(t, 5, hits[0].DenseRank)
	assert.Equal(t, 1, hits[0].FusionRank)
	assert.InDelta(t, 0.0301, hits[0].FusionScore, 1e-12)
}

// TestProjectEvidenceTraceHitsZeroFieldsForNonRRFMode verifies that when
// the retriever produced a non-RRF hit (all RRF fields zero), the
// projection leaves them zero. The omitempty json tag means these will
// not appear in the trace JSONL — verified by trace_test.go round-trip
// tests against the trace.go schema.
func TestProjectEvidenceTraceHitsZeroFieldsForNonRRFMode(t *testing.T) {
	producedAt := time.Now().UTC()
	score := 0.85
	ev, err := envelope.NewEvidence(envelope.EvidenceInput{
		SourceTitle:     "cascade-source",
		Snippet:         "cascade test snippet",
		EvidenceKind:    envelope.EvidenceKindKnowledge,
		ChunkID:         "cascade-chunk-x",
		KBVersion:       "kb-test",
		RetrievalScore:  &score,
		QueryNormalized: "test query",
		ProducedAt:      producedAt,
	})
	require.NoError(t, err)

	items := []knowledge.RetrievalHit{
		{
			Chunk: knowledge.KBChunk{ChunkID: "cascade-chunk-x"},
			Score: 0.85,
			Kept:  true,
			// RRF fields intentionally zero — represents a hit from
			// hybrid_cosine / hybrid_rerank / qwen3_full mode.
		},
	}

	hits := projectEvidenceTraceHits([]envelope.Evidence{ev}, items)
	require.Len(t, hits, 1)
	assert.Zero(t, hits[0].BM25Rank, "non-RRF modes leave BM25Rank zero")
	assert.Zero(t, hits[0].DenseRank, "non-RRF modes leave DenseRank zero")
	assert.Zero(t, hits[0].FusionRank, "non-RRF modes leave FusionRank zero")
	assert.Zero(t, hits[0].FusionScore, "non-RRF modes leave FusionScore zero")
}
