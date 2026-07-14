package engine

import (
	"testing"
	"time"

	"github.com/compshare-agent/internal/envelope"
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
		SelectedInstanceSource: SelectedInstanceSourceUser,
		// SelectedInstanceAtUnix intentionally zero (legacy row).
	}, 1)

	eng.expireStaleSelectedInstance(time.Unix(1_900_000_000, 0)) // far in the future

	state, _, _ := eng.SessionStateSnapshot()
	assert.Equal(t, "uhost-legacy", state.SelectedInstanceID, "legacy referent remains available for understanding")
	assert.Empty(t, state.SelectedInstanceSource, "unknown-age provenance must not authorize a write")
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

// TestRecordSelectedInstanceFromEnvelopeStampsTimestamp guards the second
// trusted (Source=User) writer. The envelope path (direct-dispatch monitor /
// resource-selection resume) establishes exactly the same mutating-trusted
// binding as recordSelectedInstanceID, so it MUST also start the TTL clock —
// otherwise the most common way a "current instance" is bound would carry
// SelectedInstanceAtUnix==0 and be permanently exempt from
// expireStaleSelectedInstance (indistinguishable from a pre-field legacy row).
// This test fails if the envelope writer stops stamping (e.g. reverts to inline
// field assignment).
func TestRecordSelectedInstanceFromEnvelopeStampsTimestamp(t *testing.T) {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{}, nil)
	eng.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent}, 1)

	before := time.Now().Unix()
	eng.recordSelectedInstanceFromEnvelope(&envelope.Envelope{Subjects: []envelope.Subject{
		{ID: "uhost-a", Name: "alpha", Type: envelope.SubjectInstance},
	}})
	after := time.Now().Unix()

	state, _, _ := eng.SessionStateSnapshot()
	require.Equal(t, "uhost-a", state.SelectedInstanceID)
	require.Equal(t, SelectedInstanceSourceUser, state.SelectedInstanceSource)
	assert.GreaterOrEqual(t, state.SelectedInstanceAtUnix, before, "envelope selection must start the TTL clock")
	assert.LessOrEqual(t, state.SelectedInstanceAtUnix, after)
}
