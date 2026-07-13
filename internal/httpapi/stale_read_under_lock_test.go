package httpapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The read-before-lock race, on ONE replica.
//
// I previously called this unreachable, because chatStream holds the per-session engine lease
// across the state write, so two turns of a session cannot persist concurrently. That was true
// and it was the wrong thing to check. The lease serializes the ENGINE. It does not serialize
// the READ — prepareChat fetches the session row BEFORE it takes the lease. So:
//
//	T1 streams `done` (state not yet persisted) -> the client's input box unlocks
//	T2 arrives, reads the session row -> v1, and THEN blocks on the lease
//	T1 persists v2 (a newly selected instance, a new last-intent), releases
//	T2 acquires the lease and hydrates ... v1
//
// T2 now answers on a context one turn out of date — silently — and then writes with
// expected_version=v1, hits a CAS conflict, and the retry overwrites T1's v2. A fast follow-up
// is all it takes.
//
// stalingSessions reproduces exactly that window: the row changes between the pre-lease read
// and the moment the lock is acquired, which is what the concurrent T1 does in production.
type stalingSessions struct {
	*mockSessions
	// swapOnNthGet replaces the row's context AFTER the Nth GetByID, standing in for T1
	// committing while T2 waits on the lease. The pre-lease read is the 1st Get.
	swapOnNthGet int
	gets         int
	fresh        json.RawMessage
	freshVersion int
}

func (s *stalingSessions) GetByID(ctx context.Context, owner store.Owner, sessionID string) (store.Session, error) {
	sess, err := s.mockSessions.GetByID(ctx, owner, sessionID)
	s.gets++
	if s.gets == s.swapOnNthGet {
		// T1 commits here, while T2 is blocked on the lease.
		updated := sess
		updated.Context = s.fresh
		updated.ContextVersion = s.freshVersion
		s.byID[sessionID] = updated
	}
	return sess, err
}

func TestChat_HydratesTheStateTheLastTurnPERSISTED_NotTheOneItReadBeforeTheLock(t *testing.T) {
	const staleInstance = "uhost-1exampleaa01"
	const freshInstance = "uhost-1exampleaa99"

	stale, err := json.Marshal(engine.PersistedContext{AgentSessionState: engine.SessionState{
		SchemaVersion:      engine.SessionStateSchemaCurrent,
		SelectedInstanceID: staleInstance,
	}})
	require.NoError(t, err)
	fresh, err := json.Marshal(engine.PersistedContext{AgentSessionState: engine.SessionState{
		SchemaVersion:      engine.SessionStateSchemaCurrent,
		SelectedInstanceID: freshInstance,
	}})
	require.NoError(t, err)

	sessions := &stalingSessions{
		mockSessions: &mockSessions{byID: map[string]store.Session{
			"synthetic-raced-session": {
				ID: "synthetic-raced-session", TopOrganizationID: 1, OrganizationID: 2,
				Context: stale, ContextVersion: 1, MessageCount: 2,
				CreatedAt: time.Now(), UpdatedAt: time.Now(),
			},
		}},
		// The 1st Get is prepareChat's pre-lease read. The previous turn commits right after
		// it — exactly the window a fast follow-up lands in.
		swapOnNthGet: 1,
		fresh:        fresh,
		freshVersion: 2,
	}

	eng := engine.NewWithDeps(chatLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)

	h := NewHandlers(
		&config.Config{Agent: config.AgentConfig{
			LLM:  config.LLMConfig{Model: "model-x"},
			HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: time.Hour},
			Meta: config.MetaConfig{MaxInputLength: 4000},
			STS:  config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
		}},
		sessions,
		&recordingMessages{},
		mockFeedback{},
		fakePool{eng: eng},
		nil,
	)

	_, apiErr := runChatJSON(t, h,
		`{"Action":"SendCSAgentChat","SessionId":"synthetic-raced-session","Message":"帮我重启这台","request_uuid":"req-race","top_organization_id":1,"organization_id":2}`)
	require.Nil(t, apiErr)

	// THE INVARIANT. Whatever the previous turn persisted is what this turn must be holding
	// when the model runs. Reading before the lock and hydrating that snapshot is how a user
	// gets an answer about the instance they were looking at TWO turns ago — and how the
	// previous turn's state then gets overwritten on the way out.
	got, _, hydrated := eng.SessionStateSnapshot()
	require.True(t, hydrated, "precondition: the turn hydrated some state")
	assert.Equal(t, freshInstance, got.SelectedInstanceID,
		"the turn must run on the state the PREVIOUS turn persisted, not on the copy it read before it held the lock")
}

// The write must be durable before the client is told the turn succeeded, because `done` is
// what unlocks the user's input box: sending it first is an open invitation for the next turn
// to start racing this write, which is exactly the window the test above exploits. Asserted as
// an ORDER, not a timing.
func TestChat_StateIsPersistedBeforeTheDoneFrame(t *testing.T) {
	clock := &seqClock{}
	sessions := &orderRecordingSessions{
		mockSessions: &mockSessions{byID: map[string]store.Session{
			"synthetic-order-session": {
				ID: "synthetic-order-session", TopOrganizationID: 1, OrganizationID: 2,
				ContextVersion: 1, MessageCount: 2,
				CreatedAt: time.Now(), UpdatedAt: time.Now(),
			},
		}},
		clock: clock,
	}

	eng := engine.NewWithDeps(chatLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)

	h := NewHandlers(
		&config.Config{Agent: config.AgentConfig{
			LLM:  config.LLMConfig{Model: "model-x"},
			HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: time.Hour},
			Meta: config.MetaConfig{MaxInputLength: 4000},
			STS:  config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
		}},
		sessions,
		&recordingMessages{},
		mockFeedback{},
		fakePool{eng: eng},
		nil,
	)

	base := BaseRequest{Action: "SendCSAgentChat", RequestUUID: "req-order"}
	base.Owner.TopOrganizationID = 1
	base.Owner.OrganizationID = 2

	prep, apiErr := h.prepareChat(context.Background(), base, "synthetic-order-session", "hi", "")
	require.Nil(t, apiErr)
	defer prep.release()

	sink := &orderSink{clock: clock}
	h.chatStream(context.Background(), sink, base, prep)

	require.NotZero(t, sink.doneAt, "precondition: the turn emitted done")
	require.NotZero(t, sessions.updateContextAt, "precondition: the turn persisted its state")

	assert.Less(t, sessions.updateContextAt, sink.doneAt,
		"the state must be durable BEFORE done — done unlocks the user's input, so announcing success first invites the next turn to race a write that has not landed")
}

// seqClock is a monotonic counter shared by the sink and the session store, so the two can be
// ordered against each other without depending on wall-clock timing.
type seqClock struct{ n int64 }

func (c *seqClock) next() int64 { c.n++; return c.n }

type orderSink struct {
	clock  *seqClock
	doneAt int64
}

func (s *orderSink) WriteEvent(event string, _ any) error {
	if event == "done" {
		s.doneAt = s.clock.next()
	}
	return nil
}
func (s *orderSink) WriteKeepalive() error { return nil }

type orderRecordingSessions struct {
	*mockSessions
	clock           *seqClock
	updateContextAt int64
}

func (s *orderRecordingSessions) UpdateContext(ctx context.Context, owner store.Owner, sessionID string, ctxJSON json.RawMessage, expectedVersion int) (int, error) {
	s.updateContextAt = s.clock.next()
	return s.mockSessions.UpdateContext(ctx, owner, sessionID, ctxJSON, expectedVersion)
}
