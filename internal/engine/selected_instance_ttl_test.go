package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExpireStaleSelectedInstanceRetainsIdentityButRevokesTrustAfterTTL
// verifies that expiry no longer erases conversational identity, while the
// write-authorizing provenance is still removed.
func TestExpireStaleSelectedInstanceRetainsIdentityButRevokesTrustAfterTTL(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	base := time.Unix(1_800_000_000, 0)
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "uhost-a",
		SelectedInstanceName:   "alpha",
		SelectedInstanceSource: SelectedInstanceSourceObserved,
		SelectedInstanceAtUnix: base.Unix(),
	}, 1)

	// One second past the TTL window.
	eng.expireStaleSelectedInstance(base.Add(selectedInstanceTTLSeconds*time.Second + time.Second))

	state, _, _ := eng.SessionStateSnapshot()
	assert.Equal(t, "uhost-a", state.SelectedInstanceID, "expired identity must remain available for understanding")
	assert.Equal(t, "alpha", state.SelectedInstanceName)
	assert.Empty(t, state.SelectedInstanceSource, "stale selection source must be cleared")
	assert.Equal(t, base.Unix(), state.SelectedInstanceAtUnix, "observation time must remain available")
	assert.Equal(t, ContinuityFreshnessExpired, state.SelectedInstanceFreshness)
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
		SelectedInstanceSource: SelectedInstanceSourceObserved,
		SelectedInstanceAtUnix: base.Unix(),
	}, 1)

	eng.expireStaleSelectedInstance(base.Add(selectedInstanceTTLSeconds*time.Second - time.Second))

	state, _, _ := eng.SessionStateSnapshot()
	assert.Equal(t, "uhost-a", state.SelectedInstanceID, "fresh selection must survive")
	assert.Equal(t, SelectedInstanceSourceObserved, state.SelectedInstanceSource)
	assert.Equal(t, ContinuityFreshnessStale, state.SelectedInstanceFreshness,
		"a binding in the second half of its TTL is stale but still within its authorization window")
}

// TestExpireStaleSelectedInstanceDowngradesUnstampedLegacyRow verifies rows
// persisted before the timestamp field existed keep their referent for
// conversation, but lose write-authorizing provenance because their age is
// unknowable.
func TestExpireStaleSelectedInstanceDowngradesUnstampedLegacyRow(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "uhost-legacy",
		SelectedInstanceName:   "legacy",
		SelectedInstanceSource: SelectedInstanceSourceObserved,
		// SelectedInstanceAtUnix intentionally zero (legacy row).
	}, 1)

	eng.expireStaleSelectedInstance(time.Unix(1_900_000_000, 0)) // far in the future

	state, _, _ := eng.SessionStateSnapshot()
	assert.Equal(t, "uhost-legacy", state.SelectedInstanceID, "legacy referent remains available for understanding")
	assert.Empty(t, state.SelectedInstanceSource, "unknown-age provenance must not authorize a write")
	assert.Equal(t, ContinuityFreshnessStale, state.SelectedInstanceFreshness)
}

// TestRecordObservedInstanceIDStampsTimestamp verifies recording a selection
// stamps SelectedInstanceAtUnix so the TTL clock starts. recordObservedInstanceID
// is the only writer left: the two User-sourced writers it replaces
// (recordSelectedInstanceID / recordSelectedInstanceFromEnvelope) were fed by the
// direct-dispatch lane P6 deleted, so nothing produced a "user" source any more.
func TestRecordObservedInstanceIDStampsTimestamp(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)

	before := time.Now().Unix()
	eng.recordObservedInstanceID("uhost-a", "alpha")
	after := time.Now().Unix()

	state, _, _ := eng.SessionStateSnapshot()
	require.Equal(t, "uhost-a", state.SelectedInstanceID)
	require.Equal(t, SelectedInstanceSourceObserved, state.SelectedInstanceSource)
	assert.GreaterOrEqual(t, state.SelectedInstanceAtUnix, before, "selection must be stamped at record time")
	assert.LessOrEqual(t, state.SelectedInstanceAtUnix, after)
}
