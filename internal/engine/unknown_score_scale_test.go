package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/compshare-agent/internal/knowledge"
)

// remoteHits builds a hit list on the [0,1] scale a cosine/reranker pipeline
// produces. Every score here is far below weakEvidenceBM25Threshold (55.0) and
// at or above weakEvidenceSemanticThreshold (0.5) — i.e. these are STRONG hits
// on their own scale that the BM25 floor would reject outright.
func remoteHits(scores ...float64) []knowledge.RetrievalHit {
	items := make([]knowledge.RetrievalHit, 0, len(scores))
	for i, score := range scores {
		items = append(items, knowledge.RetrievalHit{
			Chunk: knowledge.KBChunk{ChunkID: string(rune('a' + i))},
			Score: score,
			Kept:  true,
		})
	}
	return items
}

// TestUnknownRemoteScaleIsNotFlooredAsWeakEvidence is the regression this file
// exists for. A remote knowledge service that omits (or renames) its retrieval
// mode used to be recorded as bm25_only, which put a 55.0 floor in front of a
// [0,1] score and dropped EVERY hit — the ledger emptied, and downstream that
// is indistinguishable from "the corpus has nothing".
//
// The premise assertions are not decoration: without them a future change to
// either threshold could make this test pass because nothing was ever near a
// floor, rather than because the unknown scale is exempt.
func TestUnknownRemoteScaleIsNotFlooredAsWeakEvidence(t *testing.T) {
	hits := remoteHits(0.91, 0.88)

	require.Less(t, hits[0].Score, weakEvidenceBM25Threshold,
		"premise: the top hit must sit below the BM25 floor, or this test proves nothing")
	require.True(t, isWeakEvidence(hits, knowledge.RetrievalModeBM25Only, true),
		"premise: labelled bm25_only, this exact hit list IS floored — that is the old behavior")

	assert.False(t, isWeakEvidence(hits, knowledge.RetrievalModeUnknownRemote, true),
		"an unidentifiable score scale must not be judged against a guessed floor")
}

// TestKnownScalesStillFloor is the control. The fix must exempt exactly one
// value; if it disabled the floor generally, the whole weak-evidence guard would
// be gone and every assertion above would still pass.
func TestKnownScalesStillFloor(t *testing.T) {
	assert.True(t, isWeakEvidence(remoteHits(0.2), knowledge.RetrievalModeQwen3Full, true),
		"a semantic score below 0.5 is still weak evidence")
	assert.True(t, isWeakEvidence(remoteHits(3.0), knowledge.RetrievalModeBM25Only, false),
		"a BM25 score below 55.0 is still weak evidence")
	assert.True(t, isWeakEvidence(remoteHits(0.2), knowledge.RetrievalModeBM25Fallback, false),
		"bm25_fallback is a known scale and must keep being judged")
	assert.False(t, isWeakEvidence(remoteHits(0.9), knowledge.RetrievalModeQwen3Full, true),
		"a strong semantic score is not weak evidence")
}

// TestUnknownRemoteScaleReportsNoFloorValue pins the trace side. Recording 55.0
// for a floor that never ran would send an operator looking at scores instead of
// at the remote's metadata.
func TestUnknownRemoteScaleReportsNoFloorValue(t *testing.T) {
	assert.Zero(t, weakEvidenceThresholdFor(knowledge.RetrievalModeUnknownRemote),
		"no floor ran, so RetrievalTrace.FloorValue must be omitted")
	assert.Equal(t, weakEvidenceBM25Threshold, weakEvidenceThresholdFor(knowledge.RetrievalModeBM25Only),
		"control: a known mode still reports its floor")
}

// TestUnknownRemoteScaleIsNotRankingAmbiguous covers the telemetry twin. The
// BM25 spread is wide relative to a [0,1] scale, so guessing would mark nearly
// every remote turn a ranking-error candidate.
func TestUnknownRemoteScaleIsNotRankingAmbiguous(t *testing.T) {
	hits := remoteHits(0.91, 0.88)

	require.True(t, isRankingAmbiguous(hits, knowledge.RetrievalModeBM25Only),
		"premise: on the BM25 spread this pair reads as tied — that is the old behavior")

	assert.False(t, isRankingAmbiguous(hits, knowledge.RetrievalModeUnknownRemote),
		"an unidentifiable scale must not feed the ranking-ambiguity metric")
	assert.True(t, isRankingAmbiguous(remoteHits(0.91, 0.905), knowledge.RetrievalModeQwen3Full),
		"control: a known scale still detects a tie")
}
