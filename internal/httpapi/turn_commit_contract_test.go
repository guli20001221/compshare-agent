package httpapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/compshare-agent/internal/agentpool"
	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// realPoolHandlers builds Handlers over the REAL agentpool.Pool.
//
// This matters more than it looks. Every previous test of the read-under-lock ordering used a
// fake pool whose Lease returned instantly — so the ordering it claimed to verify was never
// exercised at all. A test named "..._NotTheOneItReadBeforeTheLock" stayed green with the re-read
// moved BACK to before the lock, because with an instant lease there is no "before" and "after"
// to tell apart. It counted reads. It could not see order.
//
// A real pool blocks. That is what creates the window this contract lives or dies in.
func realPoolHandlers(t *testing.T, sessions store.SessionStore, messages store.MessageStore) (*Handlers, *agentpool.Pool) {
	t.Helper()
	deps := &engine.SharedDeps{
		LLMClient:        chatLLM{},
		RateLimiter:      governance.NewInMemoryRateLimiter(governance.DefaultLimits()),
		ExternalExecutor: chatExecutor{},
	}
	pool := agentpool.NewWithDeps(deps, messages, agentpool.Options{
		Capacity: 10,
		IdleTTL:  time.Hour,
	})
	t.Cleanup(pool.Close)

	h := NewHandlers(
		&config.Config{Agent: config.AgentConfig{
			LLM:  config.LLMConfig{Model: "model-x"},
			HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: time.Hour},
			Meta: config.MetaConfig{MaxInputLength: 4000},
			STS:  config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
		}},
		sessions,
		messages,
		mockFeedback{},
		pool,
		nil,
	)
	return h, pool
}

func envelopeWith(t *testing.T, instanceID string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(engine.PersistedContext{
		AgentSessionState: engine.SessionState{
			SchemaVersion:      engine.SessionStateSchemaCurrent,
			SelectedInstanceID: instanceID,
		},
	})
	require.NoError(t, err)
	return raw
}

// The turn must reason from the state that was current WHEN IT GOT THE LOCK — not from the copy
// it read before it queued.
//
// The lease serializes the ENGINE. It does not reach back in time and serialize the read that
// happened before it. So a turn can sit blocked on the lease holding a snapshot the previous turn
// has already replaced, and then hydrate the model with it — and answer, silently, on a context
// one turn out of date. That is the amnesia, and it needs no exotic conditions: a user sending a
// follow-up while the previous answer is still streaming is enough.
//
// This test opens that exact window with a REAL pool: it holds the session's lease, lets the
// request arrive and queue behind it, changes the session row WHILE THE REQUEST IS BLOCKED, and
// then releases. The row the handler must end up reasoning from is the one written during the
// wait.
func TestChat_ReadsTheSessionAfterAcquiringTheLease_NotBefore(t *testing.T) {
	const (
		before = "uhost-1exampleaa01" // what the row says when the request arrives
		after  = "uhost-1exampleaa02" // what the previous turn commits while this one waits
	)

	sessions := &mockSessions{byID: map[string]store.Session{
		"sess-order": {
			ID:                "sess-order",
			TopOrganizationID: 1,
			OrganizationID:    2,
			Context:           envelopeWith(t, before),
			ContextVersion:    0,
			CreatedAt:         time.Now(),
			UpdatedAt:         time.Now(),
		},
	}}
	messages := &recordingMessages{}
	h, pool := realPoolHandlers(t, sessions, messages)

	// A turn is already running on this session: take its lease and hold it.
	_, release, err := pool.Lease(context.Background(), store.Owner{TopOrganizationID: 1, OrganizationID: 2}, "sess-order")
	require.NoError(t, err)

	// The next request arrives. It reads the row (still `before`), then QUEUES on the lease.
	type result struct {
		sink *recordingSink
		err  *APIError
	}
	done := make(chan result, 1)
	go func() {
		sink, apiErr := dispatchChatTurn(t, h, "sess-order", "那台机器怎么样了")
		done <- result{sink, apiErr}
	}()

	// Let it get as far as the lease and block there.
	time.Sleep(100 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("the request completed while the session's lease was HELD — the pool is not serializing this session, and nothing below this line means anything")
	default:
	}

	// The previous turn commits: a different instance is now the selected one.
	sessions.mu.Lock()
	row := sessions.byID["sess-order"]
	row.Context = envelopeWith(t, after)
	row.ContextVersion = 1
	sessions.byID["sess-order"] = row
	sessions.mu.Unlock()

	release()

	got := <-done
	require.Nil(t, got.err)

	// The turn must have COMMITTED. This is the first thing that breaks if the read moves back
	// out from under the lease, and the way it breaks is worth reading: a turn that hydrated the
	// pre-lease snapshot carries that snapshot's context_version, so its commit loses the CAS
	// against the version written while it waited, and it ends as TurnNotSaved. The read-under-
	// lease and the CAS are defence in depth — with both in place a stale read cannot silently
	// ship an answer; the worst it can do is fail loudly.
	require.True(t, got.sink.has("done"),
		"the turn must complete. If this fails with an error frame instead, the turn hydrated the copy it read BEFORE queueing on the lease: it carried a stale context_version, lost the CAS to the writer that committed while it waited, and could not save")

	// THE ASSERTION. The state this turn committed is built from the state it hydrated. If it
	// hydrated the pre-lease snapshot, it just answered about `before` — an instance the user
	// has since moved on from — and then wrote that stale selection back over the newer one.
	var pc engine.PersistedContext
	require.NoError(t, json.Unmarshal(sessions.byID["sess-order"].Context, &pc))
	assert.Equal(t, after, pc.AgentSessionState.SelectedInstanceID,
		"the turn must have hydrated the row as it stood when it ACQUIRED the lease. Seeing %q means it answered from the copy it read BEFORE queueing — one turn out of date", before)
}

// A turn that cannot confirm its context is current must not answer at all.
//
// "Fall back to the snapshot we already have" reads like graceful degradation and is not: the
// snapshot is the thing we are trying to stop trusting. Answering from a context we know we
// cannot verify is the bug, not a softer version of it. Fail, and let the user retry.
func TestChat_ReReadFailureRefusesTheTurnBeforeTheModelAnswers(t *testing.T) {
	sessions := &failingReReadSessions{mockSessions: mockSessions{byID: map[string]store.Session{
		"sess-reread": {
			ID: "sess-reread", TopOrganizationID: 1, OrganizationID: 2,
			Context: envelopeWith(t, "uhost-1exampleaa01"), ContextVersion: 0,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		},
	}}}
	messages := &recordingMessages{}
	h, _ := realPoolHandlers(t, sessions, messages)

	sink, apiErr := dispatchChatTurn(t, h, "sess-reread", "hi")

	require.NotNil(t, apiErr, "the turn must be refused, not answered from a snapshot we cannot confirm is current")
	assert.False(t, sink.has("done"))
	assert.Equal(t, 0, messages.assistantUpdates,
		"the model must not have been asked anything: the refusal happens BEFORE the turn runs, so no tokens are spent and nothing is streamed to the user")
}

// failingReReadSessions serves the first GetByID (the pre-lease read that finds the session) and
// fails every one after it — which is exactly the re-read under the lease.
type failingReReadSessions struct {
	mockSessions
	reads int
}

func (f *failingReReadSessions) GetByID(ctx context.Context, owner store.Owner, sessionID string) (store.Session, error) {
	f.mu.Lock()
	f.reads++
	n := f.reads
	f.mu.Unlock()
	if n > 1 {
		return store.Session{}, context.DeadlineExceeded
	}
	return f.mockSessions.GetByID(ctx, owner, sessionID)
}

// A turn whose commit keeps failing is reported as not saved — after a bounded retry, and never
// as success.
//
// The reply was streamed and cannot be unsent. But `done` is what unlocks the client's input box,
// and announcing it here tells the user to carry on from a conversation the server has no record
// of. The turn AFTER this one is the one that looks like the agent forgot.
func TestChat_CommitThatKeepsFailingIsReportedNotAnnouncedAsDone(t *testing.T) {
	sessions := &mockSessions{byID: map[string]store.Session{
		"sess-commit": {
			ID: "sess-commit", TopOrganizationID: 1, OrganizationID: 2,
			Context: envelopeWith(t, "uhost-1exampleaa01"), ContextVersion: 0,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		},
	}}
	attempts := 0
	sessions.updateContextOverride = func(string, json.RawMessage, int) (int, error) {
		attempts++
		return 0, context.DeadlineExceeded // a transient store fault, not a CAS conflict
	}
	messages := &recordingMessages{}
	h, _ := realPoolHandlers(t, sessions, messages)

	sink, _ := dispatchChatTurn(t, h, "sess-commit", "hi")

	assert.False(t, sink.has("done"),
		"the turn did not commit, so it must not be announced as done")
	assert.True(t, sink.has("error"),
		"a turn that could not be saved must be REPORTED — the old code logged a warning and told the client it had succeeded")
	assert.Equal(t, turnCommitAttempts, attempts,
		"a transient store fault must be retried a bounded number of times before the turn is declared lost")
}

// The retry must NOT extend to a CAS conflict. That is not a fault to ride out — it is another
// writer, and trying again can only overwrite them.
func TestChat_ACASConflictIsNotRetried(t *testing.T) {
	sessions := &mockSessions{byID: map[string]store.Session{
		"sess-cas": {
			ID: "sess-cas", TopOrganizationID: 1, OrganizationID: 2,
			Context: envelopeWith(t, "uhost-1exampleaa01"), ContextVersion: 0,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		},
	}}
	attempts := 0
	sessions.updateContextOverride = func(string, json.RawMessage, int) (int, error) {
		attempts++
		return 0, store.ErrStaleWrite
	}
	messages := &recordingMessages{}
	h, _ := realPoolHandlers(t, sessions, messages)

	sink, _ := dispatchChatTurn(t, h, "sess-cas", "hi")

	assert.Equal(t, 1, attempts,
		"exactly one attempt: a CAS conflict means someone else committed, and a retry would write our state on top of theirs — turning a conflict we can detect into state loss we cannot")
	assert.False(t, sink.has("done"))
	assert.True(t, sink.has("error"))
}
