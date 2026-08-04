package knowledge

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNormalizeRemoteScoreScale covers what the adapter is allowed to claim
// about a remote's scores. The rule is narrow on purpose: pass a mode through
// only when this build has a calibrated floor for it, and never invent one.
func TestNormalizeRemoteScoreScale(t *testing.T) {
	t.Run("known modes pass through untouched", func(t *testing.T) {
		for _, mode := range []string{
			RetrievalModeBM25Only, RetrievalModeHybridCosine, RetrievalModeHybridRerank,
			RetrievalModeQwen3Full, RetrievalModeQwen3RRF, RetrievalModeBM25Fallback,
		} {
			gotMode, gotReason := normalizeRemoteScoreScale(mode, "")
			assert.Equal(t, mode, gotMode, "known mode %q must survive normalization", mode)
			assert.Empty(t, gotReason, "a known mode is not a degradation")
		}
	})

	t.Run("an omitted mode is not silently called bm25_only", func(t *testing.T) {
		mode, reason := normalizeRemoteScoreScale("", "")
		assert.Equal(t, RetrievalModeUnknownRemote, mode)
		assert.Equal(t, "remote_mode_missing", reason)
		require.NotEqual(t, RetrievalModeBM25Only, mode,
			"the old default put a 55.0 floor in front of an unknown scale")
	})

	// The likelier case: compshare-kb owns its retrieval pipeline and can rename
	// or add a mode without this repo shipping. An unrecognized mode reaches the
	// same threshold table as an absent one, so it needs the same treatment.
	t.Run("an unrecognized mode is treated like an absent one and keeps its raw value", func(t *testing.T) {
		mode, reason := normalizeRemoteScoreScale("hybrid_v2", "")
		assert.Equal(t, RetrievalModeUnknownRemote, mode)
		assert.Equal(t, "remote_mode_unrecognized:hybrid_v2", reason,
			"the raw mode must survive, or nobody can tell which remote change disabled the floor")
	})

	t.Run("the remote's own degradation reason is not clobbered", func(t *testing.T) {
		_, reason := normalizeRemoteScoreScale("hybrid_v2", "reranker_timeout")
		assert.Equal(t, "reranker_timeout; remote_mode_unrecognized:hybrid_v2", reason)
	})

	t.Run("whitespace is not a mode", func(t *testing.T) {
		mode, reason := normalizeRemoteScoreScale("   ", "")
		assert.Equal(t, RetrievalModeUnknownRemote, mode)
		assert.Equal(t, "remote_mode_missing", reason)
	})
}

// TestKnownRetrievalModeDerivesFromTheScoreScale pins that the adapter's gate is
// not a second list. The earlier version of this test restated the same six
// modes, which cannot catch drift: whoever adds a mode to one list edits the
// other copy in the same change, and the test agrees with them either way.
//
// What is asserted instead is the derivation — classify a mode once in
// ScoreScaleFor and the gate follows.
func TestKnownRetrievalModeDerivesFromTheScoreScale(t *testing.T) {
	for _, mode := range AllRetrievalModes() {
		require.NotEqual(t, ScoreScaleUnknown, ScoreScaleFor(mode),
			"%q ships without a score scale", mode)
		assert.True(t, KnownRetrievalMode(mode), "a classified mode must pass the gate")
	}

	assert.False(t, KnownRetrievalMode("hybrid_v2"),
		"an unclassified mode must not pass, however plausible its name")
	assert.False(t, KnownRetrievalMode(RetrievalModeUnknownRemote),
		"the marker for 'no calibrated floor' must never report as judgeable")
}

// TestEmptyModeIsAFixtureArtifactNotAClaim covers the one place the two
// questions deliberately diverge. A hand-written RetrievalResult{} leaves the
// mode empty and a body of pre-mode-aware tests depends on those being judged on
// the BM25 scale — but a remote that names no mode has asserted nothing, so it
// must not be believed.
func TestEmptyModeIsAFixtureArtifactNotAClaim(t *testing.T) {
	assert.Equal(t, ScoreScaleBM25, ScoreScaleFor(""),
		"pre-mode-aware fixtures still judge on the BM25 scale")
	assert.False(t, KnownRetrievalMode(""),
		"a remote may not claim the empty mode and be believed")
}

// TestUnknownScaleIsTheZeroValue pins a safety property of the enum rather than
// a behavior: a mode nobody classified must degrade to "decline to judge", which
// only holds while ScoreScaleUnknown is what a missing switch case produces.
func TestUnknownScaleIsTheZeroValue(t *testing.T) {
	var unset ScoreScale
	assert.Equal(t, ScoreScaleUnknown, unset,
		"forgetting to classify a mode must fail safe, not fall into a calibrated scale")
}
