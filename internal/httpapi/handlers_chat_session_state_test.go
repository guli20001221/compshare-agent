package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type terminalChatLLM struct{ err error }

func (c terminalChatLLM) Chat(_ context.Context, _ llm.ChatRequest) (*llm.ChatResponse, error) {
	if c.err != nil {
		return nil, c.err
	}
	return &llm.ChatResponse{Content: "ok"}, nil
}

type refreshingPool struct {
	eng     *engine.Engine
	onLease func()
}

func (p refreshingPool) Lease(_ context.Context, _ store.Owner, _ string) (*engine.Engine, func(), error) {
	if p.onLease != nil {
		p.onLease()
	}
	return p.eng, func() {}, nil
}

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

func TestPrepareChatRefreshesSessionStateAfterWaitingForLease(t *testing.T) {
	sess := store.Session{
		ID: "sess-refresh-after-lease", TopOrganizationID: 1, OrganizationID: 2,
		ContextVersion: 0, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	sessions := &mockSessions{byID: map[string]store.Session{sess.ID: sess}}
	eng := engine.NewWithDeps(terminalChatLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)
	job := engine.PersistedInstanceOpsJob{
		InstanceID: "uhost-active", JobID: "job-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		State: "running", Purpose: "download model", UpdatedAt: "2026-08-25T12:00:00Z",
	}
	latestRaw, err := json.Marshal(engine.PersistedContext{AgentSessionState: engine.SessionState{
		SchemaVersion: engine.SessionStateSchemaV8, PersistedInstanceOpsJob: job,
	}})
	require.NoError(t, err)
	h := NewHandlers(
		&config.Config{Agent: config.AgentConfig{
			LLM:  config.LLMConfig{Model: "model-x"},
			HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: time.Hour},
			STS:  config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
		}}, sessions, &recordingMessages{}, mockFeedback{}, refreshingPool{
			eng: eng,
			onLease: func() {
				row := sessions.byID[sess.ID]
				row.Context = latestRaw
				row.ContextVersion = 4
				sessions.byID[sess.ID] = row
			},
		}, nil)
	base := BaseRequest{Action: "SendCSAgentChat", RequestUUID: "req-refresh"}
	base.Owner = store.Owner{TopOrganizationID: 1, OrganizationID: 2}

	prep, apiErr := h.prepareChat(context.Background(), base, sess.ID, "继续上一轮", "")
	require.Nil(t, apiErr)
	defer prep.release()
	state, version, hydrated := prep.agent.SessionStateSnapshot()
	require.True(t, hydrated)
	require.Equal(t, 4, version, "hydration must use the row re-read inside the session lease")
	require.Equal(t, job, state.PersistedInstanceOpsJob)
}

func TestTurnLimitAllowsOnlyBoundedActiveJobContinuation(t *testing.T) {
	job := engine.PersistedInstanceOpsJob{
		InstanceID: "uhost-active", JobID: "job-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		State: "running", Purpose: "compile requested app", UpdatedAt: "2026-08-25T12:00:00Z",
	}
	raw, err := json.Marshal(engine.PersistedContext{AgentSessionState: engine.SessionState{
		SchemaVersion: engine.SessionStateSchemaV8, PersistedInstanceOpsJob: job,
	}})
	require.NoError(t, err)
	for _, tc := range []struct {
		name         string
		messageCount int
		wantAllowed  bool
	}{
		{name: "first continuation at ordinary cap", messageCount: 6, wantAllowed: true},
		{name: "six continuation attempts exhausted", messageCount: 18, wantAllowed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := engine.NewWithDeps(terminalChatLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
			eng.RehydrateHistory(nil)
			sess := store.Session{
				ID: "sess-job-cap", TopOrganizationID: 1, OrganizationID: 2,
				Context: raw, ContextVersion: 2, MessageCount: tc.messageCount,
				CreatedAt: time.Now(), UpdatedAt: time.Now(),
			}
			h := NewHandlers(
				&config.Config{Agent: config.AgentConfig{
					LLM: config.LLMConfig{Model: "model-x"}, HTTP: config.HTTPConfig{
						MaxInputLength: 4000, SSEKeepaliveInterval: time.Hour, MaxSessionTurns: 3,
					}, STS: config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
				}}, &mockSessions{byID: map[string]store.Session{sess.ID: sess}},
				&recordingMessages{}, mockFeedback{}, fakePool{eng: eng}, nil)

			sink, apiErr := dispatchChatTurn(t, h, sess.ID, "继续后台任务")
			if tc.wantAllowed {
				require.Nil(t, apiErr)
				require.True(t, sink.has("done"))
			} else {
				require.NotNil(t, apiErr)
				require.Equal(t, ErrSessionTurnLimit.RetCode, apiErr.RetCode)
			}
		})
	}
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

	sink, _ := dispatchChatTurn(t, h, "sess-bad", "hi")

	assert.True(t, sink.has("done"),
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

// Case 5: ClearSessionState defense. Pre-hydrate the cached Engine with a
// non-empty SessionState, then run a chat whose sessions.context is
// malformed. The handler must invoke ClearSessionState immediately after
// Lease so that the cached Engine carries no sticky state into a turn
// whose parse fails.
//
// Two complementary assertions make this load-bearing:
//
//	(a) sessions.updateContextCalls == 0 — persistence skipped (already
//	    guaranteed by sessionStatePersistable, but a necessary baseline);
//	(b) eng.SessionStateSnapshot() returns hydrated=false after the turn
//	    — this is what actually proves ClearSessionState ran. Without
//	    the clear, hydrated would stay true (carrying "uhost-prev" set
//	    below), and M2's in-engine writer would step on a stale value.
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

	sink, _ := dispatchChatTurn(t, h, "sess-sticky", "hi")

	assert.True(t, sink.has("done"))
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

	sink, _ := dispatchChatTurn(t, h, "sess-stale", "hi")

	assert.True(t, sink.has("done"),
		"stream must emit done even when CAS loses on persist — reply was already streamed")
	assert.False(t, sink.has("error"),
		"ErrStaleWrite is a warning-only condition, not a stream error")
	require.Equal(t, 1, sessions.updateContextCalls)
}

func TestChatStreamEveryTerminusPersistsExistingBackgroundJob(t *testing.T) {
	job := engine.PersistedInstanceOpsJob{
		InstanceID: "uhost-job",
		JobID:      "job-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		State:      "running",
		Purpose:    "download model weights",
		UpdatedAt:  "2026-08-25T12:00:00Z",
	}
	for _, tc := range []struct {
		name     string
		llmErr   error
		cancel   bool
		wantDone bool
	}{
		{name: "success", wantDone: true},
		{name: "llm_error", llmErr: errors.New("model unavailable")},
		{name: "client_disconnect", llmErr: context.Canceled, cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(engine.PersistedContext{
				AgentSessionState: engine.SessionState{
					SchemaVersion:           engine.SessionStateSchemaV8,
					PersistedInstanceOpsJob: job,
				},
				ClientContext: json.RawMessage(`{"page":"instance"}`),
			})
			require.NoError(t, err)
			sess := store.Session{
				ID: "sess-" + tc.name, TopOrganizationID: 1, OrganizationID: 2,
				Context: raw, ContextVersion: 4, CreatedAt: time.Now(), UpdatedAt: time.Now(),
			}
			eng := engine.NewWithDeps(terminalChatLLM{err: tc.llmErr}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
			eng.RehydrateHistory(nil)
			sessions := &mockSessions{byID: map[string]store.Session{sess.ID: sess}}
			h := NewHandlers(
				&config.Config{Agent: config.AgentConfig{
					LLM:  config.LLMConfig{Model: "model-x"},
					HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: time.Hour},
					STS:  config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
				}},
				sessions, &recordingMessages{}, mockFeedback{}, fakePool{eng: eng}, nil,
			)
			base := BaseRequest{Action: "SendCSAgentChat", RequestUUID: "req-" + tc.name}
			base.Owner = store.Owner{TopOrganizationID: 1, OrganizationID: 2}
			prep, apiErr := h.prepareChat(context.Background(), base, sess.ID, "continue", "")
			require.Nil(t, apiErr)
			defer prep.release()

			streamCtx := context.Background()
			if tc.cancel {
				cancelled, cancel := context.WithCancel(context.Background())
				cancel()
				streamCtx = cancelled
			}
			sink := &recordingSink{}
			h.chatStream(streamCtx, sink, base, prep)

			require.Equal(t, tc.wantDone, sink.has("done"))
			require.Equal(t, 1, sessions.updateContextCalls,
				"the detached SessionState write must run once on every terminus")
			persisted, err := engine.ParsePersistedContext(sessions.byID[sess.ID].Context)
			require.NoError(t, err)
			require.Equal(t, job, persisted.AgentSessionState.PersistedInstanceOpsJob)
			require.JSONEq(t, `{"page":"instance"}`, string(persisted.ClientContext))
		})
	}
}

func TestSessionStateStaleWritePreservesTheWinningEnvelope(t *testing.T) {
	h, sessions, _ := newChatTestHandlers(t, store.Session{
		ID: "sess-stale-client", TopOrganizationID: 1, OrganizationID: 2,
		Context: json.RawMessage(`{"source":"old"}`), ContextVersion: 0,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	calls := 0
	sessions.updateContextOverride = func(sessionID string, ctxJSON json.RawMessage, expectedVersion int) (int, error) {
		calls++
		if calls == 1 {
			concurrent, err := json.Marshal(engine.PersistedContext{
				AgentSessionState: engine.SessionState{SchemaVersion: engine.SessionStateSchemaCurrent},
				ClientContext:     json.RawMessage(`{"source":"new"}`),
			})
			require.NoError(t, err)
			row := sessions.byID[sessionID]
			row.Context = concurrent
			row.ContextVersion = expectedVersion + 1
			sessions.byID[sessionID] = row
			return 0, store.ErrStaleWrite
		}
		t.Fatal("stale session state must never be retried as a whole-envelope overwrite")
		return 0, nil
	}

	sink, _ := dispatchChatTurn(t, h, "sess-stale-client", "hi")
	require.True(t, sink.has("done"))
	require.Equal(t, 1, sessions.updateContextCalls)
	parsed, err := engine.ParsePersistedContext(sessions.byID["sess-stale-client"].Context)
	require.NoError(t, err)
	require.JSONEq(t, `{"source":"new"}`, string(parsed.ClientContext))
}

func TestSessionStateStaleWriteDoesNotOverwriteNewUnknownSchema(t *testing.T) {
	h, sessions, _ := newChatTestHandlers(t, store.Session{
		ID: "sess-stale-future", TopOrganizationID: 1, OrganizationID: 2,
		ContextVersion: 0, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	future := json.RawMessage(`{"agent_session_state":{"schema_version":"99.0","future":true},"client_context":{"source":"future"}}`)
	sessions.updateContextOverride = func(sessionID string, _ json.RawMessage, expectedVersion int) (int, error) {
		row := sessions.byID[sessionID]
		row.Context = append(json.RawMessage(nil), future...)
		row.ContextVersion = expectedVersion + 1
		sessions.byID[sessionID] = row
		return 0, store.ErrStaleWrite
	}

	sink, _ := dispatchChatTurn(t, h, "sess-stale-future", "hi")
	require.True(t, sink.has("done"))
	require.Equal(t, 1, sessions.updateContextCalls)
	require.JSONEq(t, string(future), string(sessions.byID["sess-stale-future"].Context))
}
