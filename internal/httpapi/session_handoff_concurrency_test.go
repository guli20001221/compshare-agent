package httpapi

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A capped session does not get ONE rollover attempt. It gets one per turn that arrives while
// it is capped — a second tab, a double-submit, a retry after a timeout — and every one of them
// runs the full rollover.
//
// With a randomly generated successor id each attempt minted its OWN successor, and the
// conversation FORKED: half the user's context in one session, half in another, and the front
// end adopting whichever meta frame happened to land last. A fork is strictly worse than the
// wall it replaces — the wall at least left the user with one coherent transcript.
//
// The fix makes the successor's id a pure function of the capped session's id, so the primary
// key is what enforces at-most-one. This test races two real goroutines through the real
// rollCappedSession and pins the outcome the constraint gives us.
func TestTurnCap_TwoConcurrentTurnsGetOneSuccessor_NotAFork(t *testing.T) {
	h, sessions, _ := cappedSessionHandlers(t, engine.SessionState{
		SchemaVersion:      engine.SessionStateSchemaCurrent,
		SelectedInstanceID: "uhost-1exampleaa01",
	}, 10)
	h.SetSessionHandoffEnabled(true)

	capped := sessions.byID["sess-capped"]
	owner := store.Owner{TopOrganizationID: 1, OrganizationID: 2}

	// Two 11th turns, in flight at the same time. Both roll over; neither knows about the other.
	const racers = 2
	var wg sync.WaitGroup
	got := make([]store.Session, racers)
	errs := make([]error, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release them together, so this is a race and not a sequence
			got[i], errs[i] = h.rollCappedSession(context.Background(), owner, capped)
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < racers; i++ {
		require.NoError(t, errs[i], "racer %d: both turns must roll over — neither may be dropped", i)
	}

	// THE INVARIANT. One conversation, one continuation.
	assert.Equal(t, got[0].ID, got[1].ID,
		"both turns must land in the SAME successor — two successors is a forked conversation, and the user's context ends up split across sessions with the front end following whichever meta frame arrived last")

	require.Equal(t, racers, sessions.createCalls,
		"precondition: both racers really did attempt to create — otherwise this test proves nothing about concurrency. Counted across BOTH creation entry points, so it keeps holding if the fix is mutated back to the random-id Create.")

	successors := 0
	for id := range sessions.byID {
		if id != "sess-capped" {
			successors++
		}
	}
	assert.Equal(t, 1, successors, "exactly one successor row may exist for a capped session")

	// And the one that exists is a real successor, not a stub the loser overwrote empty.
	pc, err := engine.ParsePersistedContext(sessions.byID[got[0].ID].Context)
	require.NoError(t, err)
	require.NotNil(t, pc.Handoff)
	assert.Equal(t, "sess-capped", pc.Handoff.FromSessionID)
	assert.Len(t, pc.Handoff.Messages, engine.SessionHandoffMessages)
	assert.Equal(t, "uhost-1exampleaa01", pc.AgentSessionState.SelectedInstanceID,
		"the loser must not have clobbered the winner's carried state")
}

// The orphan. The rollover creates the successor and then goes on to lease an engine, run OCR,
// stream — any of which can fail. When it did, the successor row existed but the client was
// never told about it, so the retry created ANOTHER one: the fork above, arriving by a slower
// route, plus a trail of sessions no user will ever see.
//
// Deriving the id from the predecessor closes this too, and it closes it by making the second
// attempt IDEMPOTENT rather than by adding cleanup: the retry computes the same id, finds its
// own earlier row, and reuses it — handoff and all.
func TestTurnCap_RetryAfterAFailedRolloverReusesTheSuccessor_NotAnOrphan(t *testing.T) {
	h, sessions, _ := cappedSessionHandlers(t, engine.SessionState{
		SchemaVersion: engine.SessionStateSchemaCurrent,
		LastIntent:    "diagnosis",
	}, 10)
	h.SetSessionHandoffEnabled(true)

	capped := sessions.byID["sess-capped"]
	owner := store.Owner{TopOrganizationID: 1, OrganizationID: 2}

	// Attempt 1: the successor is created — and then imagine everything after it fails (the
	// lease times out, the stream dies). The client learns nothing.
	first, err := h.rollCappedSession(context.Background(), owner, capped)
	require.NoError(t, err)

	// Attempt 2: the user retries. The capped session is still capped, so this rolls over again.
	second, err := h.rollCappedSession(context.Background(), owner, capped)
	require.NoError(t, err)

	assert.Equal(t, first.ID, second.ID,
		"the retry must REUSE the successor the failed attempt created — minting a fresh one leaves the first as an orphan the user never sees and forks the conversation")

	successors := 0
	for id := range sessions.byID {
		if id != "sess-capped" {
			successors++
		}
	}
	assert.Equal(t, 1, successors,
		"a failed rollover must not leave a session row behind; the retry adopts it instead")

	// The reused row is intact — the second attempt must not have blanked what the first wrote.
	pc, err := engine.ParsePersistedContext(sessions.byID[second.ID].Context)
	require.NoError(t, err)
	require.NotNil(t, pc.Handoff)
	assert.Equal(t, "diagnosis", pc.AgentSessionState.LastIntent)
	assert.Equal(t, "sess-capped", pc.Handoff.FromSessionID)
}

// The successor id must be a stable, pure function of the predecessor's id. If it ever stopped
// being one — a timestamp, a counter, a random seed — at-most-one creation would silently
// revert to the fork, and every test above would keep passing because they each roll over
// within one process.
func TestSuccessorSessionID_IsAStablePureFunctionOfThePredecessor(t *testing.T) {
	a := successorSessionID("sess-capped")
	b := successorSessionID("sess-capped")
	assert.Equal(t, a, b, "the same capped session must always derive the same successor — this is what the primary key enforces at-most-once ON")
	assert.NotEqual(t, a, successorSessionID("sess-other"),
		"different capped sessions must derive different successors")
	assert.NotEqual(t, "sess-capped", a, "the successor is a NEW session, not the predecessor")

	// It has to be a valid session id for the CHAR(36) column the sessions table declares.
	assert.Len(t, a, 36, "the derived id must be a UUID — sessions.id is CHAR(36)")
}

// The handoff must never be handed to a DIFFERENT tenant, however the id is derived. The store
// reads back owner-scoped precisely so a colliding id is a not-found rather than a hand-over of
// someone else's conversation; this pins that the rollover surfaces it as an error instead of
// continuing into a stranger's session.
func TestTurnCap_ASuccessorIDHeldByAnotherTenantIsNotAdopted(t *testing.T) {
	h, sessions, _ := cappedSessionHandlers(t, engine.SessionState{
		SchemaVersion: engine.SessionStateSchemaCurrent,
	}, 10)
	h.SetSessionHandoffEnabled(true)

	capped := sessions.byID["sess-capped"]
	owner := store.Owner{TopOrganizationID: 1, OrganizationID: 2}

	// Another tenant already holds the id we would derive.
	foreign, err := json.Marshal(engine.PersistedContext{
		AgentSessionState: engine.SessionState{SchemaVersion: engine.SessionStateSchemaCurrent},
	})
	require.NoError(t, err)
	sessions.byID[successorSessionID("sess-capped")] = store.Session{
		ID:                successorSessionID("sess-capped"),
		TopOrganizationID: 999, // not ours
		OrganizationID:    999,
		Context:           foreign,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}

	_, err = h.rollCappedSession(context.Background(), owner, capped)
	require.Error(t, err,
		"an id held by another tenant must fail the rollover — continuing into their session would hand one customer's conversation to another")
}
