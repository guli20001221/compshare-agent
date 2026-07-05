package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExpireStaleSelectedInstanceClearsAfterTTL verifies a carried instance
// binding is dropped once it has gone untouched longer than the TTL, so a later
// pronoun ("它") cannot resolve to (and the trust guard cannot trust) a stale
// selection.
func TestExpireStaleSelectedInstanceClearsAfterTTL(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	base := time.Unix(1_800_000_000, 0)
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "uhost-a",
		SelectedInstanceName:   "alpha",
		SelectedInstanceSource: SelectedInstanceSourceUser,
		SelectedInstanceAtUnix: base.Unix(),
	}, 1)

	// One second past the TTL window.
	eng.expireStaleSelectedInstance(base.Add(selectedInstanceTTLSeconds*time.Second + time.Second))

	state, _, _ := eng.SessionStateSnapshot()
	assert.Empty(t, state.SelectedInstanceID, "stale selection must be cleared")
	assert.Empty(t, state.SelectedInstanceSource, "stale selection source must be cleared")
	assert.Zero(t, state.SelectedInstanceAtUnix, "stale selection timestamp must be cleared")
}

// TestExpireStaleSelectedInstanceKeepsFreshBinding verifies a recent selection
// (within the TTL) survives.
func TestExpireStaleSelectedInstanceKeepsFreshBinding(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	base := time.Unix(1_800_000_000, 0)
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "uhost-a",
		SelectedInstanceName:   "alpha",
		SelectedInstanceSource: SelectedInstanceSourceUser,
		SelectedInstanceAtUnix: base.Unix(),
	}, 1)

	eng.expireStaleSelectedInstance(base.Add(selectedInstanceTTLSeconds*time.Second - time.Second))

	state, _, _ := eng.SessionStateSnapshot()
	assert.Equal(t, "uhost-a", state.SelectedInstanceID, "fresh selection must survive")
	assert.Equal(t, SelectedInstanceSourceUser, state.SelectedInstanceSource)
}

// TestExpireStaleSelectedInstanceIgnoresUnstampedLegacyRow verifies rows
// persisted before the timestamp field existed (SelectedInstanceAtUnix == 0) are
// never auto-expired, so the rollout does not silently drop existing selections.
func TestExpireStaleSelectedInstanceIgnoresUnstampedLegacyRow(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "uhost-legacy",
		SelectedInstanceName:   "legacy",
		SelectedInstanceSource: SelectedInstanceSourceUser,
		// SelectedInstanceAtUnix intentionally zero (legacy row).
	}, 1)

	eng.expireStaleSelectedInstance(time.Unix(1_900_000_000, 0)) // far in the future

	state, _, _ := eng.SessionStateSnapshot()
	assert.Equal(t, "uhost-legacy", state.SelectedInstanceID, "unstamped legacy selection must not be auto-expired")
}

// TestRecordSelectedInstanceIDStampsTimestamp verifies recording a user
// selection stamps SelectedInstanceAtUnix so the TTL clock starts.
func TestRecordSelectedInstanceIDStampsTimestamp(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)

	before := time.Now().Unix()
	eng.recordSelectedInstanceID("uhost-a", "alpha")
	after := time.Now().Unix()

	state, _, _ := eng.SessionStateSnapshot()
	require.Equal(t, "uhost-a", state.SelectedInstanceID)
	assert.GreaterOrEqual(t, state.SelectedInstanceAtUnix, before, "selection must be stamped at record time")
	assert.LessOrEqual(t, state.SelectedInstanceAtUnix, after)
}
