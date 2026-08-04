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

// TestKnownScalesStillFloor is the control. The fix must exempt exactly the
// unjudgeable cases; if it disabled the floor generally, the whole weak-evidence
// guard would be gone and every assertion above would still pass.
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

// TestFloorValueReportsOnlyAFloorThatActuallyRan pins the trace semantic that
// FloorValue claims. Reporting a threshold nothing was compared against sends an
// operator to look at scores when the fault is elsewhere.
//
// The qwen3_rrf case is the one that mattered and the one an earlier version of
// this fix got wrong: the mode KEEPS its label through a reranker fallback while
// its scores revert to the RRF fusion scale, so the verdict skipped the 0.5
// floor while the trace still printed 0.5 beside a 0.031 score.
func TestFloorValueReportsOnlyAFloorThatActuallyRan(t *testing.T) {
	rrfScores := remoteHits(0.031, 0.028)

	t.Run("qwen3_rrf without reranker scores reports no floor", func(t *testing.T) {
		require.False(t, isWeakEvidence(rrfScores, knowledge.RetrievalModeQwen3RRF, false),
			"premise: the verdict already declines to judge RRF fusion scores")

		_, judged := appliedFloor(rrfScores, knowledge.RetrievalModeQwen3RRF, false)
		assert.False(t, judged, "no comparison happened, so FloorValue must be omitted")
	})

	t.Run("qwen3_rrf WITH reranker scores reports the semantic floor", func(t *testing.T) {
		floor, judged := appliedFloor(remoteHits(0.7), knowledge.RetrievalModeQwen3RRF, true)
		require.True(t, judged, "control: the reranker scored, so the floor does run")
		assert.Equal(t, weakEvidenceSemanticThreshold, floor)
	})

	t.Run("an unknown remote scale reports no floor", func(t *testing.T) {
		_, judged := appliedFloor(remoteHits(0.91), knowledge.RetrievalModeUnknownRemote, true)
		assert.False(t, judged)
	})

	t.Run("no hits means no comparison", func(t *testing.T) {
		_, judged := appliedFloor(nil, knowledge.RetrievalModeQwen3Full, true)
		assert.False(t, judged, "an empty or unavailable retrieval compared nothing")
	})

	t.Run("a judged query still reports its floor", func(t *testing.T) {
		floor, judged := appliedFloor(remoteHits(3.0), knowledge.RetrievalModeBM25Only, false)
		require.True(t, judged)
		assert.Equal(t, weakEvidenceBM25Threshold, floor)
	})
}

// TestVerdictAndTraceShareOneProducer is the structural assertion behind the fix
// above. Syncing conditions across two functions would have fixed the qwen3_rrf
// instance and left the shape that produced it, so what is pinned here is not a
// case list but the invariant: whenever isWeakEvidence declines to judge, the
// trace reports no floor, and whenever it judges, the trace reports the floor it
// used.
func TestVerdictAndTraceShareOneProducer(t *testing.T) {
	scales := append(knowledge.AllRetrievalModes(), knowledge.RetrievalModeUnknownRemote, "", "hybrid_v2")
	scores := [][]knowledge.RetrievalHit{nil, remoteHits(0.031), remoteHits(0.7), remoteHits(90.0)}

	for _, mode := range scales {
		for _, reranked := range []bool{true, false} {
			for _, hits := range scores {
				floor, judged := appliedFloor(hits, mode, reranked)
				weak := isWeakEvidence(hits, mode, reranked)
				if !judged {
					assert.Zero(t, floor, "mode=%q reranked=%v: no floor ran, so none may be reported", mode, reranked)
					assert.False(t, weak, "mode=%q reranked=%v: nothing was compared, so nothing is weak", mode, reranked)
					continue
				}
				assert.NotZero(t, floor, "mode=%q reranked=%v: a floor ran, so it must be reportable", mode, reranked)
				assert.Equal(t, hits[0].Score < floor, weak,
					"mode=%q reranked=%v: the verdict must be exactly the reported floor applied to the top hit", mode, reranked)
			}
		}
	}
}

// TestTheFloorExemptionDoesNotRideOnTheDisplayValue keeps the mechanisms
// separate. An earlier version of this fix expressed the exemption by returning
// 0 from weakEvidenceThresholdFor, which disabled the floor as a SIDE EFFECT of
// a display value: `score < 0` is false for any positive score. The two changes
// then covered for each other, and a mutation deleting the real guard survived
// the whole suite.
func TestTheFloorExemptionDoesNotRideOnTheDisplayValue(t *testing.T) {
	assert.NotZero(t, weakEvidenceThresholdFor(knowledge.RetrievalModeUnknownRemote),
		"the scale table must not be the thing that disables the floor; appliedFloor is, "+
			"and it has to be independently killable")
}

// TestEveryShippedModeHasACalibratedFloor is the drift guard, and it enumerates
// from the source rather than restating a list — the previous version of this
// test wrote the same six modes down a second time, which cannot catch drift
// because both copies are edited by whoever is causing it.
//
// A mode that ships without a score-scale classification lands in
// ScoreScaleUnknown, so both the adapter gate and the floor silently stop
// honoring it. That is safe-by-construction but not intended, so it fails here.
func TestEveryShippedModeHasACalibratedFloor(t *testing.T) {
	for _, mode := range knowledge.AllRetrievalModes() {
		assert.NotEqual(t, knowledge.ScoreScaleUnknown, knowledge.ScoreScaleFor(mode),
			"%q ships but has no score scale, so it would be passed through by the adapter "+
				"and then never judged — classify it in knowledge.ScoreScaleFor", mode)
		assert.True(t, knowledge.KnownRetrievalMode(mode),
			"%q ships, so a remote may claim it", mode)

		floor, judged := appliedFloor(remoteHits(0.7), mode, true)
		assert.True(t, judged, "%q ships and must be judgeable", mode)
		assert.NotZero(t, floor, "%q ships and must have a floor", mode)
	}
}
