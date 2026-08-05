package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The digest used to keep its own copy of the selected instance. Because
// mergeSemanticEntities keys on (kind, id), switching instances appended rather
// than replaced, and each copy froze the Source and Freshness it was written
// with — outliving expireStaleSelectedInstance, which only ever touches
// sessionState.
//
// Exactly ONE defect was reproduced against the pre-fix tree: multi-instance
// accumulation, which is what TestBarePronounAfterTwoOperationsBindsTheNewestPick
// and TestDigestHoldsNoSelectionCopyAtAll cover. It is reachable under the
// shipped config today and does not involve the canonical transcript.
//
// TestSinglePickPastItsTTLDoesNotBind is NOT a second defect. It passed before
// this change too — see its own comment for why — and is kept as an existing
// invariant, not as a guard for anything here.

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
// broke — the two picks each left a user_selected copy in the digest, and
// bindInstanceTarget refuses to act when more than one is carried, so the reply
// became 目标引用不唯一，请明确指定要操作的实例.
func TestBarePronounAfterTwoOperationsBindsTheNewestPick(t *testing.T) {
	now := time.Now()
	e := selectionEngine()

	e.recordSelectedInstanceIDWithSource("inst-AAA", "web-01", SelectedInstanceSourceUser)
	e.refreshConversationDigest(now)
	e.recordSelectedInstanceIDWithSource("inst-BBB", "web-02", SelectedInstanceSourceUser)
	e.refreshConversationDigest(now.Add(time.Minute))

	view := (ContextCompiler{}).CompileForTurn(e, "关掉它", "t", now.Add(time.Minute))
	binding := e.bindInstanceTarget(view)

	require.False(t, binding.conflict,
		"a superseded pick collided with the live one; the user is told 目标引用不唯一 after two operations that both worked")
	require.Equal(t, "inst-BBB", binding.id, "the newest pick must win")

	// The live selection must still be the only instance the view carries as a
	// user pick, or the assertion above could hold for the wrong reason.
	picks := 0
	for _, ent := range view.SelectedEntities {
		if ent.Kind == "instance" && ent.Source == SelectedInstanceSourceUser {
			picks++
		}
	}
	require.Equal(t, 1, picks, "expected exactly one carried user pick, got %d", picks)
}

// turnEntry is the production order: expire, then refresh, then compile.
// engine.go runs exactly this sequence at the top of every turn, and getting it
// wrong is what made an earlier probe of this area report a defect that does not
// exist — see TestSinglePickPastItsTTLDoesNotBind.
func turnEntry(e *Engine, now time.Time) AgentContext {
	e.expireStaleSelectedInstance(now)
	e.refreshConversationDigest(now)
	return (ContextCompiler{}).CompileForTurn(e, "关掉它", "t", now)
}

// An invariant, NOT a regression guard for this change: it held before the fix
// too, and restoring the old writer does not break it.
//
// It is here because it looks like it should have been broken, and the reason it
// was not is worth recording. With one instance the digest copy shares its
// (kind, id) key with the live value, so the refresh that runs immediately after
// expireStaleSelectedInstance on every turn entry OVERWRITES the copy with the
// expired state — mergeSemanticEntities assigns on key match rather than
// appending. The copy could not outlive the original because it was never a
// second entry.
//
// A probe that called expireStaleSelectedInstance WITHOUT the
// refreshConversationDigest that always follows it at turn entry reported the
// opposite, and it was wrong — the probe skipped a step the runtime always
// takes.
//
// The real defect needs two DIFFERENT ids, which never key-collide and so are
// never rewritten: TestBarePronounAfterTwoOperationsBindsTheNewestPick and
// TestDigestHoldsNoSelectionCopyAtAll.
func TestSinglePickPastItsTTLDoesNotBind(t *testing.T) {
	base := time.Now()
	e := selectionEngine()

	e.recordSelectedInstanceIDWithSource("inst-AAA", "web-01", SelectedInstanceSourceUser)
	e.refreshConversationDigest(base)

	later := base.Add(time.Duration(selectedInstanceTTLSeconds+60) * time.Second)
	view := turnEntry(e, later)

	// Non-vacuity: the TTL must actually have fired, or this proves nothing.
	require.Equal(t, ContinuityFreshnessExpired, e.sessionState.SelectedInstanceFreshness)

	binding := e.bindInstanceTarget(view)
	require.Empty(t, binding.id,
		"a pick older than selectedInstanceTTLSeconds still bound a bare pronoun")
	require.False(t, binding.conflict, "an expired pick should simply not bind, not become a conflict")
}

// Why elapsed time is irrelevant once a switch has happened, stated without a
// clock.
//
// A copy is only ever rewritten while its instance is still the live selection —
// mergeSemanticEntities assigns on (kind, id) match. The moment the user moves to
// a different instance, the previous copy is unreachable by any later refresh and
// keeps its Source and Freshness for the life of the session, through expiry and
// through a restart. So the fix is not "expire the copies"; it is that the digest
// must hold none, which is what this asserts.
//
// A clock-based version of this cannot be written honestly here:
// recordSelectedInstanceIDWithSource stamps SelectedInstanceAtUnix from the real
// wall clock while expire/refresh/compile take an injected now, so advancing the
// injected clock past a TTL also expires a selection recorded moments ago.
func TestDigestHoldsNoSelectionCopyAtAll(t *testing.T) {
	now := time.Now()
	e := selectionEngine()

	e.recordSelectedInstanceIDWithSource("inst-AAA", "web-01", SelectedInstanceSourceUser)
	e.refreshConversationDigest(now)
	e.recordSelectedInstanceIDWithSource("inst-BBB", "web-02", SelectedInstanceSourceUser)
	e.refreshConversationDigest(now.Add(time.Minute))

	for _, hint := range e.sessionState.ConversationDigest.EntityHints {
		require.NotEqual(t, SelectedInstanceSourceUser, hint.Source,
			"the digest kept a selection copy (%s); nothing will ever rewrite or expire it", hint.ID)
	}
}

// The digest is persisted, so sessions poisoned before this fix carry the copies
// in the database. Stopping the writer alone would leave them there across
// restarts and replica changes with nothing to clear them.
func TestAlreadyPersistedSelectionHintsAreCleanedOnRefresh(t *testing.T) {
	now := time.Now()
	e := selectionEngine()
	// A digest as an old binary left it: two frozen picks, both fresh forever.
	e.sessionState.ConversationDigest.EntityHints = []SemanticEntityHint{
		{Kind: "instance", ID: "inst-AAA", Name: "web-01", Source: SelectedInstanceSourceUser, Freshness: ContinuityFreshnessFresh},
		{Kind: "instance", ID: "inst-BBB", Name: "web-02", Source: SelectedInstanceSourceUser, Freshness: ContinuityFreshnessFresh},
	}

	e.refreshConversationDigest(now)

	require.Empty(t, e.sessionState.ConversationDigest.EntityHints,
		"a session poisoned by an older binary still carries its stale picks")

	view := (ContextCompiler{}).CompileForTurn(e, "关掉它", "t", now)
	require.False(t, e.bindInstanceTarget(view).conflict)
}

// The cleanup keys on Source, and the two vocabularies do not overlap:
// user_selected is written only by recordSelectedInstanceIDWithSource, while
// TaskSnapshot entities carry an actionresolver CandidateSource through
// ContextFrame.SlotSources. Completed-task entities are the digest's only unique
// contribution to SelectedEntities — CompileForTurn appends TaskSnapshot.Entities
// directly only while the task is unresolved — so losing them would be a real
// regression, not a cleanup.
func TestTaskEntitiesSurviveTheSelectionHintCleanup(t *testing.T) {
	now := time.Now()
	e := selectionEngine()
	e.sessionState.ConversationDigest.EntityHints = []SemanticEntityHint{
		{Kind: "instance", ID: "inst-AAA", Source: SelectedInstanceSourceUser, Freshness: ContinuityFreshnessFresh},
		{Kind: "instance", ID: "inst-TASK", Source: "user_explicit", Freshness: ContinuityFreshnessFresh},
		{Kind: "image", ID: "img-1", Source: "verified_context", Freshness: ContinuityFreshnessFresh},
		{Kind: "instance", ID: "inst-OBS", Source: "tool_observation", Freshness: ContinuityFreshnessStale},
	}

	e.refreshConversationDigest(now)

	got := map[string]string{}
	for _, hint := range e.sessionState.ConversationDigest.EntityHints {
		got[hint.ID] = hint.Source
	}
	require.Equal(t, map[string]string{
		"inst-TASK": "user_explicit",
		"img-1":     "verified_context",
		"inst-OBS":  "tool_observation",
	}, got, "the cleanup must remove only user_selected copies")

	// They no longer reach AgentContext at all. CompileForTurn used to append
	// ConversationDigest.EntityHints to SelectedEntities; that feed is deleted with
	// the rest of the digest projection.
	//
	// Which is safe, and this is the re-verification rather than a citation of the
	// comment on dropCarriedSelectionHints: the binder's allowlist accepts only
	// user_selected and the account-single source, and the card's accepts those two
	// plus the pending card and an observed referent. Digest hints carry
	// actionresolver CandidateSource values — user_explicit / verified_context /
	// tool_observation — so neither surface ever accepted them. Removing the feed
	// deletes the CLASS of defect the cleanup above was written to heal: a digest
	// copy can no longer reach bindInstanceTarget even in a session an older binary
	// poisoned.
	view := (ContextCompiler{}).CompileForTurn(e, "看看", "t", now)
	for _, ent := range view.SelectedEntities {
		require.NotContains(t, []string{"inst-TASK", "img-1", "inst-OBS"}, ent.ID,
			"a digest-carried hint reached the compiled view: %+v", ent)
	}
	require.False(t, e.bindInstanceTarget(view).conflict,
		"and the write path is unaffected: these sources were never bindable")
}

// A single live pick must still bind — the fix removes a duplicate, not the
// feature. Without this, every assertion above is satisfied by a binder that
// never binds anything.
func TestSingleLivePickStillBindsABarePronoun(t *testing.T) {
	now := time.Now()
	e := selectionEngine()
	e.recordSelectedInstanceIDWithSource("inst-AAA", "web-01", SelectedInstanceSourceUser)
	e.refreshConversationDigest(now)

	view := (ContextCompiler{}).CompileForTurn(e, "关掉它", "t", now)
	binding := e.bindInstanceTarget(view)

	require.False(t, binding.conflict)
	require.Equal(t, "inst-AAA", binding.id)
}
