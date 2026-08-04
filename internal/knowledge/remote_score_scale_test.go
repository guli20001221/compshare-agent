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

// TestKnownRetrievalModeMatchesTheEngineFloorTable guards the one invariant that
// makes the split safe: KnownRetrievalMode is the adapter's claim that a floor
// exists for a mode, and internal/engine's weakEvidenceThresholdFor is where
// that floor actually lives. If a mode is added to one and not the other, the
// adapter starts passing through a scale nothing can judge.
//
// The engine cannot be imported here (it depends on this package), so the check
// is stated as the list itself — a reviewer changing either side has to change
// this line, and the engine-side control test asserts the same six modes floor.
func TestKnownRetrievalModeMatchesTheEngineFloorTable(t *testing.T) {
	judgeable := []string{
		RetrievalModeBM25Only, RetrievalModeHybridCosine, RetrievalModeHybridRerank,
		RetrievalModeQwen3Full, RetrievalModeQwen3RRF, RetrievalModeBM25Fallback,
	}
	for _, mode := range judgeable {
		assert.True(t, KnownRetrievalMode(mode), "%q must be judgeable", mode)
	}
	assert.False(t, KnownRetrievalMode(RetrievalModeUnknownRemote),
		"the marker for 'no calibrated floor' must never report as judgeable")
	assert.False(t, KnownRetrievalMode(""), "an absent mode is not judgeable")
	assert.False(t, KnownRetrievalMode("hybrid_v2"), "an uncalibrated mode is not judgeable")
}
