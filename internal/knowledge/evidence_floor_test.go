package knowledge

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSharedEvidenceFloorUsesTheProducingScoreScale(t *testing.T) {
	semantic := []RetrievalHit{{Score: 0.2}}
	bm25 := []RetrievalHit{{Score: 10}}

	assert.True(t, IsWeakEvidence(semantic, RetrievalModeHybridCosine, false))
	assert.True(t, IsWeakEvidence(bm25, RetrievalModeBM25Only, false))
	assert.False(t, IsWeakEvidence([]RetrievalHit{{Score: 0.9}}, RetrievalModeHybridCosine, false))
	assert.False(t, IsWeakEvidence([]RetrievalHit{{Score: 80}}, RetrievalModeBM25Only, false))
}

func TestSharedEvidenceFloorDoesNotGuessUnknownOrRawRRFScores(t *testing.T) {
	hits := []RetrievalHit{{Score: 0.01}}

	assert.False(t, IsWeakEvidence(hits, "future_score_scale", false))
	assert.False(t, IsWeakEvidence(hits, RetrievalModeQwen3RRF, false))
	assert.True(t, IsWeakEvidence(hits, RetrievalModeQwen3RRF, true),
		"a qwen3 RRF result with a reranker score is on the calibrated semantic scale")
}
