package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PRODUCTION MUST GET THE TRANSACTION.
//
// TurnCommitterFor picks the strongest commit the given stores can support, and that choice is
// invisible at the call site — which is exactly the kind of thing that silently degrades. If a
// refactor ever splits the two stores across connections, or wraps one in a decorator that hides
// DB(), production drops to the sequenced committer and the partial-write window reopens without
// a single test going red. So the choice is pinned here.
//
// sql.Open does not dial, so this needs no database.
func TestTurnCommitterFor_ProductionStoresGetTheTransaction(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://u:p@127.0.0.1:1/db?sslmode=disable")
	require.NoError(t, err)
	defer db.Close()

	// Exactly what cmd/server.go builds.
	tc := TurnCommitterFor(NewSessionStore(db), NewMessageStore(db))

	_, ok := tc.(*PostgresTurnStore)
	assert.True(t, ok,
		"the server builds both stores from ONE *sql.DB, so the two halves of a turn must commit in one transaction. Falling back to the sequenced committer here would reopen the partial-write window in production, silently")
}

// Two different databases cannot share a transaction, so the sequenced committer is the honest
// answer — not a transaction that would silently commit only one half.
func TestTurnCommitterFor_SeparateDatabasesFallBackToSequenced(t *testing.T) {
	db1, err := sql.Open("postgres", "postgres://u:p@127.0.0.1:1/a?sslmode=disable")
	require.NoError(t, err)
	defer db1.Close()
	db2, err := sql.Open("postgres", "postgres://u:p@127.0.0.1:1/b?sslmode=disable")
	require.NoError(t, err)
	defer db2.Close()

	tc := TurnCommitterFor(NewSessionStore(db1), NewMessageStore(db2))
	_, ok := tc.(sequencedTurnCommitter)
	assert.True(t, ok, "stores on different connections must not pretend to share a transaction")
}

// ---------------------------------------------------------------------------
// The contract both implementations must satisfy.
// ---------------------------------------------------------------------------

type fakeSessions struct {
	SessionStore
	version    int
	ctx        json.RawMessage
	casCalls   int
	forceError error
}

func (f *fakeSessions) UpdateContext(_ context.Context, _ Owner, _ string, ctxJSON json.RawMessage, expected int) (int, error) {
	f.casCalls++
	if f.forceError != nil {
		return 0, f.forceError
	}
	if expected != f.version {
		return 0, ErrStaleWrite
	}
	f.version++
	f.ctx = ctxJSON
	return f.version, nil
}

type fakeMessages struct {
	MessageStore
	writes int
	patch  AssistantPatch
}

func (f *fakeMessages) UpdateAssistant(_ context.Context, _ Owner, _ string, patch AssistantPatch) error {
	f.writes++
	f.patch = patch
	return nil
}

// A LOST CAS MUST LEAVE THE ANSWER UNWRITTEN.
//
// This is the half-write that mattered. If the answer lands while the state does not, the
// transcript shows the agent selecting an instance and the state says it never did — so the next
// turn reasons from the stale state and contradicts the conversation the user is looking at. That
// is the amnesia, arriving through a partial write rather than a lost one.
//
// The transaction gets this for free (the rollback takes the message with it). The sequenced
// committer gets it from ORDER: it CASes the session FIRST, so a conflict means the message write
// never runs. Reversing those two lines would satisfy every other test in this file and reopen
// the bug.
func TestSequencedTurnCommitter_ALostCASWritesNeitherHalf(t *testing.T) {
	sessions := &fakeSessions{version: 7} // the row has moved on
	messages := &fakeMessages{}
	tc := sequencedTurnCommitter{sessions: sessions, messages: messages}

	_, err := tc.CommitTurn(context.Background(), Owner{1, 2}, "sess-1", "msg-1",
		AssistantPatch{Content: "答案", Status: "ok"}, json.RawMessage(`{}`), 5) // we still think it is 5

	require.ErrorIs(t, err, ErrStaleWrite)
	assert.Equal(t, 0, messages.writes,
		"the CAS lost, so the ANSWER must not be written either. An answer stored on top of another writer's state is the partial write this type exists to prevent")
}

func TestSequencedTurnCommitter_BothHalvesLandOnSuccess(t *testing.T) {
	sessions := &fakeSessions{version: 5}
	messages := &fakeMessages{}
	tc := sequencedTurnCommitter{sessions: sessions, messages: messages}

	newVer, err := tc.CommitTurn(context.Background(), Owner{1, 2}, "sess-1", "msg-1",
		AssistantPatch{Content: "答案", Status: "ok"}, json.RawMessage(`{"a":1}`), 5)

	require.NoError(t, err)
	assert.Equal(t, 6, newVer)
	assert.Equal(t, 1, messages.writes)
	assert.Equal(t, "答案", messages.patch.Content)
	assert.JSONEq(t, `{"a":1}`, string(sessions.ctx))
}

// A transient store fault must surface as itself, NOT as a stale write — the caller retries a
// fault and refuses to retry a conflict, so confusing the two would have it overwrite a winner.
func TestSequencedTurnCommitter_ATransientFaultIsNotAStaleWrite(t *testing.T) {
	boom := errors.New("connection reset")
	sessions := &fakeSessions{version: 5, forceError: boom}
	messages := &fakeMessages{}
	tc := sequencedTurnCommitter{sessions: sessions, messages: messages}

	_, err := tc.CommitTurn(context.Background(), Owner{1, 2}, "sess-1", "msg-1",
		AssistantPatch{Status: "ok"}, json.RawMessage(`{}`), 5)

	require.ErrorIs(t, err, boom)
	assert.NotErrorIs(t, err, ErrStaleWrite,
		"a transient fault is retried and a CAS conflict is not — reporting one as the other makes the caller overwrite whoever won")
	assert.Equal(t, 0, messages.writes)
}
