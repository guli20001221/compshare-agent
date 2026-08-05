package engine

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This file used to be about ConversationDigest.EntityHints: the digest kept its
// own copy of the selected instance, mergeSemanticEntities keyed on (kind, id) so
// switching instances APPENDED rather than replaced, and each copy froze the
// Source and Freshness it was written with — outliving expireStaleSelectedInstance,
// which only ever touches sessionState. Two successful operations therefore left
// two user_selected entries and the next bare 关掉它 answered 目标引用不唯一.
//
// That storage no longer exists. The defect is now unreachable by construction
// rather than by cleanup, and SessionState has no field that could hold such a
// copy. What survives here is the BEHAVIOUR those tests were protecting, which is
// about live selection and is unaffected by the digest's removal — plus one new
// test for the only way a poisoned copy could still arrive: a row written by an
// older binary.
//
// TestSinglePickPastItsTTLDoesNotBind was never a regression guard for any of
// this; it is an invariant, kept for the reason its own comment gives.

// selectionEngine is a hydrated engine with no history — the smallest thing that
// can hold a selection and compile a context view.
func selectionEngine() *Engine {
	e := &Engine{}
	e.sessionStateHydrated = true
	return e
}

// Two SUCCESSFUL operations, then a bare pronoun.
//
// Neither operation is blocked: both name their instance explicitly and bind
// through the binder's explicit-reference tier. It is the turn AFTER them that
// broke.
func TestBarePronounAfterTwoOperationsBindsTheNewestPick(t *testing.T) {
	now := time.Now()
	e := selectionEngine()

	e.recordSelectedInstanceIDWithSource("inst-AAA", "web-01", SelectedInstanceSourceUser)
	e.recordSelectedInstanceIDWithSource("inst-BBB", "web-02", SelectedInstanceSourceUser)

	view := (ContextCompiler{}).CompileForTurn(e, "关掉它", "t", now)
	binding := e.bindInstanceTarget(view)

	require.False(t, binding.conflict,
		"a superseded pick collided with the live one; the user is told 目标引用不唯一 after two operations that both worked")
	require.Equal(t, "inst-BBB", binding.id, "the newest pick must win")

	// The live selection must be the only instance the view carries as a user
	// pick, or the assertion above could hold for the wrong reason.
	picks := 0
	for _, ent := range view.SelectedEntities {
		if ent.Kind == "instance" && ent.Source == SelectedInstanceSourceUser {
			picks++
		}
	}
	require.Equal(t, 1, picks, "expected exactly one carried user pick, got %d", picks)
}

// turnEntry is the production order: expire, then compile. The digest refresh that
// used to sit between them is gone.
func turnEntry(e *Engine, now time.Time) AgentContext {
	e.expireStaleSelectedInstance(now)
	return (ContextCompiler{}).CompileForTurn(e, "关掉它", "t", now)
}

// An invariant, NOT a regression guard: it held before the digest was removed and
// it holds after.
//
// It is here because it looks like it should have been broken by the old digest
// copy, and the reason it was not is worth keeping. With ONE instance the copy
// shared its (kind, id) key with the live value, so the refresh that ran
// immediately after expireStaleSelectedInstance overwrote it with the expired
// state — the copy could not outlive the original because it was never a second
// entry. A probe that called expireStaleSelectedInstance without that refresh
// reported the opposite and was wrong: it skipped a step the runtime always took.
// The real defect needed two DIFFERENT ids, which never key-collide.
func TestSinglePickPastItsTTLDoesNotBind(t *testing.T) {
	base := time.Now()
	e := selectionEngine()

	e.recordSelectedInstanceIDWithSource("inst-AAA", "web-01", SelectedInstanceSourceUser)

	later := base.Add(time.Duration(selectedInstanceTTLSeconds+60) * time.Second)
	view := turnEntry(e, later)

	// Non-vacuity: the TTL must actually have fired, or this proves nothing.
	require.Equal(t, ContinuityFreshnessExpired, e.sessionState.SelectedInstanceFreshness)

	binding := e.bindInstanceTarget(view)
	require.Empty(t, binding.id,
		"a pick older than selectedInstanceTTLSeconds still bound a bare pronoun")
	require.False(t, binding.conflict, "an expired pick should simply not bind, not become a conflict")
}

// The one route by which a poisoned copy could still arrive: a session row written
// by a binary that had the digest. Deleting the writer does nothing about rows
// already in the database, and this is the assertion that could not be made from
// our own types — a round-trip test cannot produce input its own writer can no
// longer construct, so the fixture is hand-authored old wire format.
func TestSessionRowFromAnOlderBinaryDropsItsDigest(t *testing.T) {
	// Exactly what an old binary persisted for a session that had run two
	// mutating operations: two frozen user_selected picks, plus the semantic
	// blocks and the compactor's sourced memory. selected_instance_at_unix is
	// stamped live because the binder reads it against the wall clock — a literal
	// would make this test pass or fail by calendar date.
	//
	// The field VALUES are taken from real rows, not from the Go constants: the
	// replay database holds "user_selected" and "observed" for
	// selected_instance_source. Writing the plausible-looking "user" here made this
	// test fail against a binder that was working correctly, which is the whole
	// point of using a foreign fixture rather than marshalling our own types.
	raw := json.RawMessage(fmt.Sprintf(`{"agent_session_state":{
		"schema_version":"7.0",
		"selected_instance_id":"inst-BBB",
		"selected_instance_source":"user_selected",
		"selected_instance_at_unix":%d,
		"selected_instance_freshness":"fresh",
		"conversation_digest":{
			"narrative":"目标：给训练机扩容",
			"goals":["给训练机扩容"],
			"decisions":["采用第二种方案"],
			"entity_hints":[
				{"kind":"instance","id":"inst-AAA","name":"web-01","source":"user_selected","freshness":"fresh"},
				{"kind":"instance","id":"inst-BBB","name":"web-02","source":"user_selected","freshness":"fresh"}
			],
			"sources":{"decisions":[{"value":"采用第二种方案","pair_index":1,"quote":"第二种"}]},
			"excerpts":[{"user":"继续","assistant":"请确认实例"}],
			"summary_frontier":8
		}
	}}`, time.Now().Unix()))

	pc, err := ParsePersistedContext(raw)
	require.NoError(t, err,
		"an unknown field must not fail the decode — that would make every pre-cut session unloadable")
	require.Equal(t, SessionStateSchemaV7, pc.AgentSessionState.SchemaVersion)
	require.Equal(t, "inst-BBB", pc.AgentSessionState.SelectedInstanceID,
		"premise: the rest of the row must survive, or this proves only that decoding failed")

	// Re-serialising must not carry the digest back out.
	out, err := json.Marshal(pc.AgentSessionState)
	require.NoError(t, err)
	require.NotContains(t, string(out), "conversation_digest")
	require.NotContains(t, string(out), "inst-AAA",
		"the frozen pick must be gone, not merely unrendered")

	// And the poisoned session binds cleanly on its next turn.
	e := selectionEngine()
	e.sessionState = pc.AgentSessionState
	view := (ContextCompiler{}).CompileForTurn(e, "关掉它", "t", time.Now())
	binding := e.bindInstanceTarget(view)
	require.False(t, binding.conflict,
		"a session poisoned by an older binary still reports 目标引用不唯一")
	require.Equal(t, "inst-BBB", binding.id)
}

// A single live pick must still bind — the removal takes away a duplicate, not the
// feature. Without this, every assertion above is satisfied by a binder that never
// binds anything.
func TestSingleLivePickStillBindsABarePronoun(t *testing.T) {
	now := time.Now()
	e := selectionEngine()
	e.recordSelectedInstanceIDWithSource("inst-AAA", "web-01", SelectedInstanceSourceUser)

	view := (ContextCompiler{}).CompileForTurn(e, "关掉它", "t", now)
	binding := e.bindInstanceTarget(view)

	require.False(t, binding.conflict)
	require.Equal(t, "inst-AAA", binding.id)
}
