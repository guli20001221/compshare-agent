package httpapi

import (
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

// ---------------------------------------------------------------------------
// M1 SessionState persistence — handler integration tests.
//
// Each test exercises a different leg of the §6.1 / §6.2 contract in the
// M1 PR design:
//
//   1. happy path:      successful parse → SetSessionState → UpdateContext
//                       called once with envelope shape + version+1
//   2. malformed JSON:  chat finishes, UpdateContext NOT called
//   3. unknown schema:  ErrUnknownSessionStateSchema → UpdateContext NOT called
//   4. legacy upgrade:  pre-M1 raw blob → first turn rewrites as envelope,
//                       client_context preserved verbatim, version=1
//   5. ClearSessionState defense: a hypothetical sticky hydrated=true from
//                       a prior turn must NOT cause persistence when the
//                       current turn's parse fails
//   6. ErrStaleWrite:   SSE still emits done even when CAS loses
//
// All tests reuse the chatLLM / chatExecutor fakes from handlers_chat_test.go
// (same package) and assert against mockSessions.updateContextCalls /
// .lastUpdateContext.
// ---------------------------------------------------------------------------

func newChatTestHandlers(t *testing.T, sess store.Session) (*Handlers, *mockSessions, *engine.Engine) {
	t.Helper()
	eng := engine.NewWithDeps(chatLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)

	sessions := &mockSessions{byID: map[string]store.Session{sess.ID: sess}}

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
	return h, sessions, eng
}

func dispatchChatTurn(t *testing.T, h *Handlers, sessionID, message string) (*recordingSink, *APIError) {
	t.Helper()
	body := `{"Action":"SendCSAgentChat","SessionId":"` + sessionID + `","Message":"` + message + `","request_uuid":"req-1","top_organization_id":1,"organization_id":2}`
	return runChatJSON(t, h, body)
}

// Case 1: happy path — empty Context, first chat turn writes envelope with
// version+1.
func TestDispatchChat_PersistsEnvelopeOnSuccess(t *testing.T) {
	h, sessions, _ := newChatTestHandlers(t, store.Session{
		ID:                "sess-happy",
		TopOrganizationID: 1,
		OrganizationID:    2,
		ContextVersion:    0,
		// Context: nil — first turn
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	})

	sink, _ := dispatchChatTurn(t, h, "sess-happy", "hi")

	assert.True(t, sink.has("done"))
	require.Equal(t, 1, sessions.updateContextCalls,
		"expected exactly one UpdateContext call on successful turn")

	// Envelope shape and version.
	row := sessions.byID["sess-happy"]
	assert.Equal(t, 1, row.ContextVersion, "context_version must advance 0 → 1")

	var pc engine.PersistedContext
	require.NoError(t, json.Unmarshal(row.Context, &pc))
	assert.Equal(t, engine.SessionStateSchemaCurrent, pc.AgentSessionState.SchemaVersion)
	// M1 has no in-engine writer, so SelectedInstanceID stays empty.
	assert.Empty(t, pc.AgentSessionState.SelectedInstanceID)
}

// Case 2: malformed JSON in sessions.context — the state is RESET and the reset
// IS persisted, so the session heals.
//
// ⚠️ CONTRACT CHANGE (2026-07-14). This test used to be
// TestDispatchChat_MalformedContext_SkipsPersist and asserted the opposite:
// updateContextCalls == 0, broken row left in place. That assertion was pinning
// a bug, not a contract.
//
// The old reasoning ("never overwrite a row we could not read") is correct for
// an UNKNOWN SCHEMA — the data is intact, a newer binary can still read it, so
// writing would destroy it. It is wrong for MALFORMED JSON: nothing in that row
// is recoverable by anyone. Skipping the write meant the row stayed broken, so
// every subsequent turn re-failed the same way — the session was permanently
// amnesiac and could never recover on its own. Resetting to an empty state and
// persisting it costs nothing (the old bytes were already unreadable) and the
// session is healthy again from the very next turn.
//
// Case 3 (unknown schema) still asserts skip-persist. The two cases are
// deliberately not symmetric; see prepareChat.
func TestDispatchChat_MalformedContext_ResetsAndPersists_SoTheSessionHeals(t *testing.T) {
	h, sessions, _ := newChatTestHandlers(t, store.Session{
		ID:                "sess-bad",
		TopOrganizationID: 1,
		OrganizationID:    2,
		Context:           json.RawMessage(`{not valid`),
		ContextVersion:    7,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	})

	sink, _ := dispatchChatTurn(t, h, "sess-bad", "hi")

	assert.True(t, sink.has("done"),
		"chat must complete even when prior context is unparseable")
	require.Equal(t, 1, sessions.updateContextCalls,
		"a broken row must be REWRITTEN, not left broken — otherwise the session re-fails forever")

	row := sessions.byID["sess-bad"]
	assert.Equal(t, 8, row.ContextVersion, "context_version must advance 7 → 8")

	var pc engine.PersistedContext
	require.NoError(t, json.Unmarshal(row.Context, &pc),
		"the healed row must be a parseable envelope — this is the whole point of the reset")
	assert.Equal(t, engine.SessionStateSchemaCurrent, pc.AgentSessionState.SchemaVersion)
	assert.Empty(t, pc.AgentSessionState.SelectedInstanceID,
		"the reset state must be EMPTY — we recovered nothing from the broken row and must not invent anything")
}

// Case 3: unknown schema_version (forward-rollout protection) — chat
// completes, NO persistence so a newer binary can later read the row.
func TestDispatchChat_UnknownSchemaVersion_SkipsPersist(t *testing.T) {
	futureEnvelope := json.RawMessage(`{"agent_session_state":{"schema_version":"9.0","future_field":"hello"},"client_context":{"app":"console"}}`)
	h, sessions, _ := newChatTestHandlers(t, store.Session{
		ID:                "sess-future",
		TopOrganizationID: 1,
		OrganizationID:    2,
		Context:           futureEnvelope,
		ContextVersion:    3,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	})

	sink, _ := dispatchChatTurn(t, h, "sess-future", "hi")

	assert.True(t, sink.has("done"))
	assert.Equal(t, 0, sessions.updateContextCalls,
		"unknown schema_version must NOT trigger UpdateContext — leave the row for the newer binary to read")
	// Row unchanged.
	assert.JSONEq(t, string(futureEnvelope), string(sessions.byID["sess-future"].Context))
	assert.Equal(t, 3, sessions.byID["sess-future"].ContextVersion)
}

// Case 4: legacy raw client blob in sessions.context — first successful
// turn wraps it as client_context inside an envelope; version=1.
func TestDispatchChat_LegacyContextUpgradedToEnvelope(t *testing.T) {
	legacy := json.RawMessage(`{"source":"console","theme":"dark"}`)
	h, sessions, _ := newChatTestHandlers(t, store.Session{
		ID:                "sess-legacy",
		TopOrganizationID: 1,
		OrganizationID:    2,
		Context:           legacy,
		ContextVersion:    0,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	})

	_, _ = dispatchChatTurn(t, h, "sess-legacy", "hi")

	require.Equal(t, 1, sessions.updateContextCalls)

	row := sessions.byID["sess-legacy"]
	assert.Equal(t, 1, row.ContextVersion, "legacy upgrade must increment version 0 → 1")

	var pc engine.PersistedContext
	require.NoError(t, json.Unmarshal(row.Context, &pc))
	assert.Equal(t, engine.SessionStateSchemaCurrent, pc.AgentSessionState.SchemaVersion)
	assert.JSONEq(t, string(legacy), string(pc.ClientContext),
		"legacy client blob must be preserved verbatim as client_context")
}

// Case 5: ClearSessionState defense, now with real teeth.
//
// Pre-hydrate the cached Engine with SelectedInstanceID="uhost-prev" (as a
// prior turn on the same pooled Engine would), then run a turn whose
// sessions.context is malformed. The handler must call ClearSessionState right
// after Lease, so the previous session's instance does not leak into this one.
//
// ⚠️ CONTRACT CHANGE (2026-07-14): under the old skip-persist behaviour this
// test could only assert hydrated==false — a weak gate, because persistence was
// skipped anyway and nothing would have been written even if the clear were
// missing. Now that a malformed row IS rewritten, a missing ClearSessionState
// would PERSIST "uhost-prev" into a session it never belonged to. The
// assertion below is therefore a genuine gate: delete ClearSessionState from
// prepareChat and this test fails on the persisted envelope.
func TestDispatchChat_PreHydratedEngine_MalformedContext_MustNotPersistTheStaleInstance(t *testing.T) {
	h, sessions, eng := newChatTestHandlers(t, store.Session{
		ID:                "sess-sticky",
		TopOrganizationID: 1,
		OrganizationID:    2,
		Context:           json.RawMessage(`{not valid`),
		ContextVersion:    5,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	})

	// Simulate a prior turn having hydrated the Engine.
	eng.SetSessionState(engine.SessionState{
		SchemaVersion:      engine.SessionStateSchemaV1,
		SelectedInstanceID: "uhost-prev",
	}, 5)
	_, _, hydrated := eng.SessionStateSnapshot()
	require.True(t, hydrated, "test precondition: prior turn left hydrated=true")

	sink, _ := dispatchChatTurn(t, h, "sess-sticky", "hi")

	assert.True(t, sink.has("done"))
	require.Equal(t, 1, sessions.updateContextCalls,
		"a malformed row is reset and rewritten (see Case 2)")

	var pc engine.PersistedContext
	require.NoError(t, json.Unmarshal(sessions.byID["sess-sticky"].Context, &pc))
	assert.Empty(t, pc.AgentSessionState.SelectedInstanceID,
		"the pooled Engine's prior instance must NOT be persisted into this session — "+
			"mutation: delete agent.ClearSessionState() from prepareChat and this fails with uhost-prev")
}

// Case 6: UpdateContext returns ErrStaleWrite — SSE still emits done.
// The assistant reply is already delivered; CAS loss only loses the next
// turn's "previous instance" memory.
func TestDispatchChat_StaleWriteOnPersist_StillEmitsDone(t *testing.T) {
	h, sessions, _ := newChatTestHandlers(t, store.Session{
		ID:                "sess-stale",
		TopOrganizationID: 1,
		OrganizationID:    2,
		ContextVersion:    0,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	})
	sessions.updateContextOverride = func(string, json.RawMessage, int) (int, error) {
		return 0, store.ErrStaleWrite
	}

	sink, _ := dispatchChatTurn(t, h, "sess-stale", "hi")

	assert.True(t, sink.has("done"),
		"stream must emit done even when CAS loses on persist — reply was already streamed")
	assert.False(t, sink.has("error"),
		"ErrStaleWrite is a warning-only condition, not a stream error")
	require.Equal(t, 2, sessions.updateContextCalls)
}
