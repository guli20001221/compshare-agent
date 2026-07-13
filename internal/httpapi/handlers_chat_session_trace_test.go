package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the session-attribution block (internal/observability/session.go)
// end to end through the real chat path. Each one fails if the wiring regresses,
// and each failure would restore a specific blind spot:
//
//   - a silent session swap becoming invisible again (the backend runs the turn on
//     a fresh empty session while the front end still shows the old transcript, so
//     the turn looks like "the agent forgot" and is really "the agent was never
//     given the conversation");
//   - swapped:false vanishing from a normal turn (false and "not written" become
//     indistinguishable, and the swap RATE — the whole point — loses its
//     denominator);
//   - a cold DB rebuild being reported as a live pool hit (they are not equivalent:
//     a rebuild restores persisted user/assistant TEXT only, never the tool results
//     or retrieved evidence a follow-up refers to).

const sessionTraceMaxTurns = 10

// newSessionTraceHandlers builds the chat handlers with a trace writer attached,
// so the assertions read the record the production sink would have received.
func newSessionTraceHandlers(t *testing.T, sessions *mockSessions, pool EnginePool) (*Handlers, *captureTraceWriter) {
	t.Helper()
	traceWriter := &captureTraceWriter{}
	h := NewHandlers(
		&config.Config{Agent: config.AgentConfig{
			LLM: config.LLMConfig{Model: "model-x"},
			HTTP: config.HTTPConfig{
				MaxInputLength:       4000,
				SSEKeepaliveInterval: time.Hour,
				MaxSessionTurns:      sessionTraceMaxTurns,
			},
			Meta: config.MetaConfig{MaxInputLength: 4000},
			STS:  config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
		}},
		sessions,
		&recordingMessages{},
		mockFeedback{},
		pool,
		traceWriter,
	)
	return h, traceWriter
}

func newSessionTraceEngine(t *testing.T) *engine.Engine {
	t.Helper()
	eng := engine.NewWithDeps(chatLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)
	return eng
}

func onlySessionTrace(t *testing.T, w *captureTraceWriter) observability.SessionTrace {
	t.Helper()
	require.Len(t, w.records, 1, "expected exactly one trace record for the turn")
	return w.records[0].Session
}

// A turn whose SessionId is unknown to the store runs on a REPLACEMENT session the
// client never asked for. That is the failure mode the block exists to make
// countable, so it must be recorded as a swap — with both ids present but HASHED,
// because a session id is a customer identifier and the trace is exported.
func TestChatTrace_UnknownSessionIsRecordedAsSwap(t *testing.T) {
	sessions := &mockSessions{byID: map[string]store.Session{}}
	h, traceWriter := newSessionTraceHandlers(t, sessions, fakePool{eng: newSessionTraceEngine(t)})

	_, apiErr := runChatJSON(t, h, `{"Action":"SendCSAgentChat","SessionId":"synthetic-unknown-session","Message":"hi","request_uuid":"req-swap","top_organization_id":1,"organization_id":2}`)
	require.Nil(t, apiErr)

	sess := onlySessionTrace(t, traceWriter)
	assert.True(t, sess.Swapped, "an unknown SessionId mints a fresh empty session — that is a swap")
	assert.Equal(t, observability.SessionSwapNotFound, sess.SwapReason)
	assert.NotEmpty(t, sess.RequestedSessionIDHash)
	assert.NotEmpty(t, sess.SessionIDHash)
	assert.NotEqual(t, sess.RequestedSessionIDHash, sess.SessionIDHash,
		"the turn ran on a different session than the client asked for; the hashes must show it")

	// Redaction: the raw ids must not survive anywhere in the record.
	raw, err := json.Marshal(traceWriter.records[0])
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "synthetic-unknown-session")
	assert.NotContains(t, string(raw), "sess-new")

	// The replacement session is empty, so there is nothing to restore — "new", not
	// a cold rebuild. absent (client sent no id) is a normal first turn and must
	// never be confused with this.
	assert.Equal(t, observability.RehydrateSourceNew, sess.RehydrateSource)
	assert.Zero(t, sess.RehydratedMessageCount)
}

// A normal continuing turn: no swap. The block must still be PRESENT and still
// carry swapped — false is a measurement, not an absence.
func TestChatTrace_ContinuingTurnRecordsNoSwapAndKeepsTheFlag(t *testing.T) {
	sessions := &mockSessions{byID: map[string]store.Session{
		"synthetic-live-session": {
			ID:                "synthetic-live-session",
			TopOrganizationID: 1,
			OrganizationID:    2,
			MessageCount:      4, // two completed turns
			ContextVersion:    3,
			CreatedAt:         time.Now(),
			UpdatedAt:         time.Now(),
		},
	}}
	pool := fakePool{eng: newSessionTraceEngine(t), poolHit: true}
	h, traceWriter := newSessionTraceHandlers(t, sessions, pool)

	_, apiErr := runChatJSON(t, h, `{"Action":"SendCSAgentChat","SessionId":"synthetic-live-session","Message":"hi","request_uuid":"req-live","top_organization_id":1,"organization_id":2}`)
	require.Nil(t, apiErr)

	sess := onlySessionTrace(t, traceWriter)
	assert.False(t, sess.Swapped)
	assert.Empty(t, sess.SwapReason, "no swap ⇒ no reason to invent")
	assert.Equal(t, sess.RequestedSessionIDHash, sess.SessionIDHash,
		"the turn ran on exactly the session the client asked for")

	// swapped:false must survive serialization, or the swap rate loses its
	// denominator (deliberately NOT omitempty).
	raw, err := json.Marshal(traceWriter.records[0])
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"swapped":false`)

	// Turn budget: this is turn 3 of the configured 10. A turn AT the cap is amnesia
	// by design, so both the index and the cap must be on the record.
	assert.Equal(t, 3, sess.TurnIndexInSession)
	assert.Equal(t, sessionTraceMaxTurns, sess.MaxSessionTurns)

	// Served from the live pool: full in-memory history, nothing rehydrated.
	assert.Equal(t, observability.RehydrateSourceHot, sess.RehydrateSource)
	assert.Zero(t, sess.RehydratedMessageCount)

	// State durability: the turn read version 3 and successfully wrote 4.
	assert.Equal(t, 3, sess.ContextVersionIn)
	assert.Equal(t, 4, sess.ContextVersionOut)
	assert.False(t, sess.CASConflict)
	assert.False(t, sess.StateSaveFailed)
}

// The pool evicts at capacity (LRU) or after the idle TTL, and the next turn is
// rebuilt from the DB. That rebuild restores persisted user/assistant text ONLY —
// tool results and retrieved evidence were never persisted — so a cold turn must
// never be reported as a hot one.
func TestChatTrace_ColdRebuildIsDistinguishableFromHot(t *testing.T) {
	sessions := &mockSessions{byID: map[string]store.Session{
		"synthetic-evicted-session": {
			ID:                "synthetic-evicted-session",
			TopOrganizationID: 1,
			OrganizationID:    2,
			MessageCount:      4,
			ContextVersion:    1,
			CreatedAt:         time.Now(),
			UpdatedAt:         time.Now(),
		},
	}}
	// poolHit=false with prior history ⇒ the engine was rebuilt from the DB.
	pool := fakePool{eng: newSessionTraceEngine(t), poolHit: false, rehydrated: 4}
	h, traceWriter := newSessionTraceHandlers(t, sessions, pool)

	_, apiErr := runChatJSON(t, h, `{"Action":"SendCSAgentChat","SessionId":"synthetic-evicted-session","Message":"hi","request_uuid":"req-cold","top_organization_id":1,"organization_id":2}`)
	require.Nil(t, apiErr)

	sess := onlySessionTrace(t, traceWriter)
	assert.Equal(t, observability.RehydrateSourceCold, sess.RehydrateSource,
		"a DB rebuild of a session with history is cold, never hot")
	assert.Equal(t, 4, sess.RehydratedMessageCount,
		"how much the rebuild restored is the measure of what it could NOT restore")
	assert.False(t, sess.Swapped, "an eviction is not a swap — the session id is still the client's")
}

// The CAS write can lose, and today that only logs a warning while the user has
// already been told the turn succeeded. Both facts must reach the trace, or a
// silent context loss stays silent.
func TestChatTrace_CASConflictAndFailedSaveAreRecorded(t *testing.T) {
	sessions := &mockSessions{byID: map[string]store.Session{
		"synthetic-cas-session": {
			ID:                "synthetic-cas-session",
			TopOrganizationID: 1,
			OrganizationID:    2,
			MessageCount:      2,
			ContextVersion:    5,
			CreatedAt:         time.Now(),
			UpdatedAt:         time.Now(),
		},
	}}
	// Every UpdateContext (the first write AND the post-conflict retry) loses.
	sessions.updateContextOverride = func(string, json.RawMessage, int) (int, error) {
		return 0, store.ErrStaleWrite
	}
	pool := fakePool{eng: newSessionTraceEngine(t), poolHit: true}
	h, traceWriter := newSessionTraceHandlers(t, sessions, pool)

	sink, apiErr := runChatJSON(t, h, `{"Action":"SendCSAgentChat","SessionId":"synthetic-cas-session","Message":"hi","request_uuid":"req-cas","top_organization_id":1,"organization_id":2}`)
	require.Nil(t, apiErr)
	assert.True(t, sink.has("done"),
		"unchanged behavior: a lost CAS write still streams done — that is exactly why it needs a trace")

	sess := onlySessionTrace(t, traceWriter)
	assert.Equal(t, 5, sess.ContextVersionIn, "the version this turn READ")
	assert.True(t, sess.CASConflict, "the optimistic-lock write collided")
	assert.True(t, sess.StateSaveFailed,
		"the retry lost too — the user was told the turn succeeded and the state did not persist")
	assert.Zero(t, sess.ContextVersionOut,
		"nothing was written, so version_out stays unobserved — never a guessed default")
}

// Zero behavior change: the session block must be counts, flags and hashes only —
// no session id, no user text. This walks the emitted JSON for the literals the
// turn actually carried.
func TestChatTrace_SessionBlockCarriesNoRawIdentifiers(t *testing.T) {
	sessions := &mockSessions{byID: map[string]store.Session{
		"synthetic-redaction-session": {
			ID:                "synthetic-redaction-session",
			TopOrganizationID: 1,
			OrganizationID:    2,
			CreatedAt:         time.Now(),
			UpdatedAt:         time.Now(),
		},
	}}
	h, traceWriter := newSessionTraceHandlers(t, sessions, fakePool{eng: newSessionTraceEngine(t), poolHit: true})

	_, apiErr := runChatJSON(t, h, `{"Action":"SendCSAgentChat","SessionId":"synthetic-redaction-session","Message":"hi","request_uuid":"req-redact","top_organization_id":1,"organization_id":2}`)
	require.Nil(t, apiErr)

	sess := onlySessionTrace(t, traceWriter)
	require.NotEmpty(t, sess.SessionIDHash)
	assert.True(t, strings.HasPrefix(sess.SessionIDHash, "sha256:"),
		"session ids enter the trace only as hashes (same helper as UserMsgHash)")
	assert.NotContains(t, sess.SessionIDHash, "synthetic-redaction-session")

	raw, err := json.Marshal(traceWriter.records[0])
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "synthetic-redaction-session")
}
