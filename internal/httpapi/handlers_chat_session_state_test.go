package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
	"github.com/gin-gonic/gin"
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

func dispatchChatTurn(t *testing.T, h *Handlers, sessionID, message string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/gateway",
		strings.NewReader(`{"Action":"SendCSAgentChat","SessionId":"`+sessionID+`","Message":"`+message+`","request_uuid":"req-1","top_organization_id":1,"organization_id":2}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")
	h.Dispatch(c)
	return rec
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

	rec := dispatchChatTurn(t, h, "sess-happy", "hi")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "event: done")
	require.Equal(t, 1, sessions.updateContextCalls,
		"expected exactly one UpdateContext call on successful turn")

	// Envelope shape and version.
	row := sessions.byID["sess-happy"]
	assert.Equal(t, 1, row.ContextVersion, "context_version must advance 0 → 1")

	var pc engine.PersistedContext
	require.NoError(t, json.Unmarshal(row.Context, &pc))
	assert.Equal(t, engine.SessionStateSchemaV1, pc.AgentSessionState.SchemaVersion)
	// M1 has no in-engine writer, so SelectedInstanceID stays empty.
	assert.Empty(t, pc.AgentSessionState.SelectedInstanceID)
}

// Case 2: malformed JSON in sessions.context — chat completes, NO persistence.
func TestDispatchChat_MalformedContext_SkipsPersist(t *testing.T) {
	h, sessions, _ := newChatTestHandlers(t, store.Session{
		ID:                "sess-bad",
		TopOrganizationID: 1,
		OrganizationID:    2,
		Context:           json.RawMessage(`{not valid`),
		ContextVersion:    7,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	})

	rec := dispatchChatTurn(t, h, "sess-bad", "hi")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "event: done",
		"chat must complete even when prior context is unparseable")
	assert.Equal(t, 0, sessions.updateContextCalls,
		"malformed context must NOT trigger UpdateContext — would overwrite the broken row")
	// Row still has the original broken context — no permanent corruption upgrade.
	assert.Equal(t, json.RawMessage(`{not valid`), sessions.byID["sess-bad"].Context)
	assert.Equal(t, 7, sessions.byID["sess-bad"].ContextVersion)
}

// Case 3: unknown schema_version (forward-rollout protection) — chat
// completes, NO persistence so a newer binary can later read the row.
func TestDispatchChat_UnknownSchemaVersion_SkipsPersist(t *testing.T) {
	futureEnvelope := json.RawMessage(`{"agent_session_state":{"schema_version":"2.0","future_field":"hello"},"client_context":{"app":"console"}}`)
	h, sessions, _ := newChatTestHandlers(t, store.Session{
		ID:                "sess-future",
		TopOrganizationID: 1,
		OrganizationID:    2,
		Context:           futureEnvelope,
		ContextVersion:    3,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	})

	rec := dispatchChatTurn(t, h, "sess-future", "hi")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "event: done")
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

	rec := dispatchChatTurn(t, h, "sess-legacy", "hi")

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, sessions.updateContextCalls)

	row := sessions.byID["sess-legacy"]
	assert.Equal(t, 1, row.ContextVersion, "legacy upgrade must increment version 0 → 1")

	var pc engine.PersistedContext
	require.NoError(t, json.Unmarshal(row.Context, &pc))
	assert.Equal(t, engine.SessionStateSchemaV1, pc.AgentSessionState.SchemaVersion)
	assert.JSONEq(t, string(legacy), string(pc.ClientContext),
		"legacy client blob must be preserved verbatim as client_context")
}

// Case 5: ClearSessionState defense. Pre-hydrate the cached Engine with a
// non-empty SessionState, then run a chat whose sessions.context is
// malformed. The handler must invoke ClearSessionState immediately after
// Lease so that the cached Engine carries no sticky state into a turn
// whose parse fails.
//
// Two complementary assertions make this load-bearing:
//   (a) sessions.updateContextCalls == 0 — persistence skipped (already
//       guaranteed by sessionStatePersistable, but a necessary baseline);
//   (b) eng.SessionStateSnapshot() returns hydrated=false after the turn
//       — this is what actually proves ClearSessionState ran. Without
//       the clear, hydrated would stay true (carrying "uhost-prev" set
//       below), and M2's in-engine writer would step on a stale value.
func TestDispatchChat_PreHydratedEngine_MalformedContext_StillSkipsPersist(t *testing.T) {
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

	rec := dispatchChatTurn(t, h, "sess-sticky", "hi")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "event: done")
	assert.Equal(t, 0, sessions.updateContextCalls,
		"malformed parse must skip persist regardless of prior engine state")
	assert.Equal(t, json.RawMessage(`{not valid`), sessions.byID["sess-sticky"].Context)
	assert.Equal(t, 5, sessions.byID["sess-sticky"].ContextVersion)

	// The load-bearing assertion: hydrated must be false after the turn,
	// proving the handler ran ClearSessionState. Without that call, the
	// prior turn's "uhost-prev" state would remain on the engine.
	postState, _, postHydrated := eng.SessionStateSnapshot()
	assert.False(t, postHydrated,
		"handler must call ClearSessionState after Lease so cached Engine state does not leak across turns")
	assert.Equal(t, engine.SessionState{}, postState,
		"ClearSessionState must zero the SessionState struct")
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

	rec := dispatchChatTurn(t, h, "sess-stale", "hi")

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "event: done",
		"SSE must emit done even when CAS loses on persist — reply was already streamed")
	assert.NotContains(t, body, "event: error",
		"ErrStaleWrite is a warning-only condition, not a stream error")
	require.Equal(t, 1, sessions.updateContextCalls)
}
