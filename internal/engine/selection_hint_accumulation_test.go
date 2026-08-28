package engine

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Selection continuity has one current storage location: SessionState. These
// tests ensure a newer selection replaces the old one, stale selections cannot
// authorize a bare reference, and legacy persisted fields are ignored.

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

// turnEntry preserves the production order: expire, then compile.
func turnEntry(e *Engine, now time.Time) AgentContext {
	e.expireStaleSelectedInstance(now)
	return (ContextCompiler{}).CompileForTurn(e, "关掉它", "t", now)
}

// A genuine user selection is conversation-scoped. A long pause changes its
// observability freshness to stale but must not make a bare continuation lose
// the selected instance.
func TestSinglePickPastItsTTLStillBinds(t *testing.T) {
	base := time.Now()
	e := selectionEngine()

	e.recordSelectedInstanceIDWithSource("inst-AAA", "web-01", SelectedInstanceSourceUser)

	later := base.Add(time.Duration(selectedInstanceTTLSeconds+60) * time.Second)
	view := turnEntry(e, later)

	// Non-vacuity: the wall-clock threshold must actually have fired.
	require.Equal(t, ContinuityFreshnessStale, e.sessionState.SelectedInstanceFreshness)

	binding := e.bindInstanceTarget(view)
	require.Equal(t, "inst-AAA", binding.id,
		"elapsed time alone turned a genuine user selection into lost context")
	require.False(t, binding.conflict)
}

// Older rows may contain fields the current schema no longer models. Decode them
// without losing current execution state, and never write the unknown fields back.
func TestSessionRowFromAnOlderBinaryDropsItsDigest(t *testing.T) {
	// Hand-author the foreign wire shape because current types cannot emit it.
	// Keep the timestamp live so the binder does not expire the retained state.
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

	// Re-serialising must not carry unknown legacy fields back out.
	out, err := json.Marshal(pc.AgentSessionState)
	require.NoError(t, err)
	require.NotContains(t, string(out), "conversation_digest")
	require.NotContains(t, string(out), "inst-AAA",
		"the frozen pick must be gone, not merely unrendered")

	// The retained current selection still binds cleanly.
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
