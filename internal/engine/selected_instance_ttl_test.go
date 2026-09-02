package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExpireStaleSelectedInstanceRetainsIdentityButRevokesTrustAfterTTL
// verifies that expiry no longer erases conversational identity or its origin,
// while freshness still revokes automatic execution authority.
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
	assert.Equal(t, SelectedInstanceSourceObserved, state.SelectedInstanceSource,
		"origin is historical fact; freshness, not deletion, revokes authority")
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
// conversation, but are expired for authorization because their age is
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
	assert.Equal(t, SelectedInstanceSourceObserved, state.SelectedInstanceSource,
		"origin remains observable, while expired freshness cannot authorize a write")
	assert.Equal(t, ContinuityFreshnessExpired, state.SelectedInstanceFreshness)
}

// An old user selection without its timestamp cannot prove that it came through
// the current server-owned selection path. Same-conversation continuity applies
// only to stamped user_selected state.
func TestUnstampedUserSelectionCannotSupplyWorkflowBinding(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "uhost-legacy-user",
		SelectedInstanceSource: SelectedInstanceSourceUser,
		// SelectedInstanceAtUnix intentionally zero (legacy row).
	}, 1)
	preExpiryView := (ContextCompiler{}).CompileForTurn(eng, "继续排查", "turn-legacy-user-pre", time.Now())
	require.Len(t, preExpiryView.SelectedEntities, 1)
	require.Equal(t, ContinuityFreshnessExpired, preExpiryView.SelectedEntities[0].Freshness,
		"a context compiled before turn entry must not present an unstampable selection as fresh or stale authority")
	eng.expireStaleSelectedInstance(time.Now())
	view := (ContextCompiler{}).CompileForTurn(eng, "继续排查", "turn-legacy-user", time.Now())
	eng.turnContextViewThisTurn = view
	eng.turnContextViewReady = true

	binding := eng.bindInstanceTarget(view)
	require.False(t, binding.bound(), "an unstamped legacy pick is never automatic authority")
}

// A passive read must not replace the user's current target, even when it observes
// another instance after a long pause. Only a new explicit user designation may
// switch a conversation-scoped selection.
func TestObservedReadCannotReplaceLongPausedUserSelection(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	then := time.Now().Add(-(selectedInstanceTTLSeconds + 60) * time.Second).Unix()
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "uhost-a",
		SelectedInstanceName:   "alpha",
		SelectedInstanceSource: SelectedInstanceSourceUser,
		SelectedInstanceAtUnix: then,
	}, 1)
	eng.expireStaleSelectedInstance(time.Now())

	eng.recordObservedInstanceID("uhost-b", "beta")

	state, _, _ := eng.SessionStateSnapshot()
	assert.Equal(t, "uhost-a", state.SelectedInstanceID)
	assert.Equal(t, "alpha", state.SelectedInstanceName)
	assert.Equal(t, SelectedInstanceSourceUser, state.SelectedInstanceSource)
	assert.Equal(t, then, state.SelectedInstanceAtUnix, "a read must not reset the selection timestamp")
	assert.Equal(t, ContinuityFreshnessStale, state.SelectedInstanceFreshness)
}

func TestLongPausedUserSelectionRemainsStaleButBindable(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	base := time.Now().Add(-(selectedInstanceTTLSeconds + 60) * time.Second)
	eng.SetSessionState(SessionState{
		SchemaVersion:             SessionStateSchemaCurrent,
		SelectedInstanceID:        "uhost-a",
		SelectedInstanceName:      "alpha",
		SelectedInstanceSource:    SelectedInstanceSourceUser,
		SelectedInstanceAtUnix:    base.Unix(),
		SelectedInstanceFreshness: ContinuityFreshnessExpired, // persisted by an older binary
	}, 1)

	eng.expireStaleSelectedInstance(time.Now())
	view := (ContextCompiler{}).CompileForTurn(eng, "为什么启动不了", "turn-long-pause", time.Now())
	binding := eng.bindInstanceTarget(view)

	require.Equal(t, ContinuityFreshnessStale, eng.sessionState.SelectedInstanceFreshness)
	require.Equal(t, "uhost-a", binding.id)
	require.False(t, binding.explicit)
}

// The callers that re-record an ALREADY known instance as a user selection — an
// explicit SSH target or a confirmed platform write target — have only the id in hand.
// A rehydrated or post-mutation registry resolves nothing, so without a fallback
// the re-record blanks a name the session already knew and the context card can
// then name the box only by id, which reads as the agent having forgotten it.
func TestRecordingTheSameInstanceWithNoNameKeepsTheKnownName(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{
		SchemaVersion:          SessionStateSchemaCurrent,
		SelectedInstanceID:     "cpod-x",
		SelectedInstanceName:   "clip-trainer",
		SelectedInstanceSource: SelectedInstanceSourceUser,
		SelectedInstanceAtUnix: time.Now().Unix(),
	}, 1)
	// The registry is deliberately left cold: nothing can resolve cpod-x to a name.
	eng.recordSelectedInstanceIDWithSource("cpod-x", "", SelectedInstanceSourceUser)

	state, _, _ := eng.SessionStateSnapshot()
	assert.Equal(t, "clip-trainer", state.SelectedInstanceName,
		"a nameless re-record of the same id must not blank the known name")
	assert.Equal(t, ContinuityFreshnessFresh, state.SelectedInstanceFreshness)

	// A DIFFERENT instance carries no such history, so its name stays empty rather
	// than inheriting the previous box's.
	eng.recordSelectedInstanceIDWithSource("cpod-y", "", SelectedInstanceSourceUser)
	state, _, _ = eng.SessionStateSnapshot()
	assert.Equal(t, "cpod-y", state.SelectedInstanceID)
	assert.Empty(t, state.SelectedInstanceName, "a new target must never inherit the previous instance's name")
}

// TestRecordObservedInstanceIDStampsTimestamp verifies recording a selection
// stamps SelectedInstanceAtUnix so the TTL clock starts.
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
