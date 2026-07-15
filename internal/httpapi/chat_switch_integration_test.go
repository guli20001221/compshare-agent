package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/agentpool"
	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// httpITMessagesDDL mirrors the messages table from deploy/migrations/0001_init.sql
// so the switch-context integration test is self-contained.
const httpITMessagesDDL = `
CREATE TABLE IF NOT EXISTS messages (
  id            CHAR(36)     NOT NULL PRIMARY KEY,
  session_id    CHAR(36)     NOT NULL,
  request_uuid  VARCHAR(64),
  role          VARCHAR(16)  NOT NULL,
  content       TEXT         NOT NULL,
  status        VARCHAR(16)  NOT NULL,
  error_code    VARCHAR(64),
  model         VARCHAR(64),
  input_tokens  INT,
  output_tokens INT,
  ttft_ms       INT,
  latency_ms    INT,
  metadata      JSONB,
  created_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_session_created ON messages (session_id, created_at);
CREATE INDEX IF NOT EXISTS idx_request_uuid ON messages (request_uuid)`

// TestChatSwitchSessions_CarriesCorrectContext_Integration is the end-to-end
// proof that switching conversations carries the CORRECT per-session context.
//
// It wires the REAL agentpool.Pool (Capacity 1, so leasing B evicts A and a
// later lease of A re-rehydrates from the DB — not a shared cache) to a REAL
// MySQL-backed MessageStore + SessionStore, seeds two sessions of the SAME owner
// with DISTINCT transcripts and DISTINCT persisted SessionState, then replays
// the A → B → A switch sequence and asserts each lease carries its own
// transcript + SessionState with zero bleed in either direction.
//
// It mirrors prepareChat's hydration order (Lease → ClearSessionState →
// ParsePersistedContext → SetSessionState, handlers_chat.go:223-258) up to but
// not including the LLM call: message rehydration and SessionState hydration
// both complete BEFORE ChatWithOptions dials the model, so the carried context
// is fully assertable without any network. (refreshSystemPrompt later injects
// the asserted SessionState into the system prompt at ChatWithOptions start.)
//
// Gated on COMPSHARE_TEST_MYSQL_DSN; skipped in the normal suite; refuses the
// production host.
func TestChatSwitchSessions_CarriesCorrectContext_Integration(t *testing.T) {
	db := openHTTPTestDB(t)
	defer db.Close()
	ctx := context.Background()

	_, err := db.ExecContext(ctx, httpITSessionsDDL)
	require.NoError(t, err, "create sessions table")
	_, err = db.ExecContext(ctx, httpITMessagesDDL)
	require.NoError(t, err, "create messages table")

	owner := store.Owner{TopOrganizationID: 4294967000, OrganizationID: 4294967001}
	const (
		sessA = "switch-test-session-a"
		sessB = "switch-test-session-b"
	)
	cleanup := func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM messages WHERE session_id IN ($1, $2)", sessA, sessB)
		_, _ = db.ExecContext(ctx, "DELETE FROM sessions WHERE top_organization_id = $1", owner.TopOrganizationID)
	}
	cleanup()
	t.Cleanup(cleanup)

	sessionStore := store.NewSessionStore(db)
	messageStore := store.NewMessageStore(db)

	// envelope builds a persisted-context JSON carrying a session-specific
	// SelectedInstanceID, exactly as the chat-turn persist path would write it.
	envelope := func(instanceID string) json.RawMessage {
		raw, mErr := json.Marshal(engine.PersistedContext{
			AgentSessionState: engine.SessionState{
				SchemaVersion:      engine.SessionStateSchemaV1,
				SelectedInstanceID: instanceID,
			},
		})
		require.NoError(t, mErr)
		return raw
	}

	// Seed two sessions of the SAME owner with distinct context envelopes.
	seedSession := func(id, instanceID string) {
		_, sErr := db.ExecContext(ctx,
			"INSERT INTO sessions (id, top_organization_id, organization_id, context, context_version) VALUES ($1, $2, $3, $4, 1)",
			id, owner.TopOrganizationID, owner.OrganizationID, string(envelope(instanceID)))
		require.NoError(t, sErr, "seed session %s", id)
	}
	seedSession(sessA, "uhost-AAA")
	seedSession(sessB, "uhost-BBB")

	// Seed distinct transcripts. Each "ok" turn must rehydrate into its own
	// session; the "pending" draft in A must be dropped by filterHistory and
	// never appear in any engine's history.
	seedMsg := func(id, sessionID, role, content, status string) {
		require.NoError(t, messageStore.Append(ctx, store.Message{
			ID: id, SessionID: sessionID, Role: role, Content: content, Status: status,
		}))
	}
	seedMsg("m-a-1", sessA, "user", "我的实例是 uhost-AAA，帮我盯着它", "ok")
	seedMsg("m-a-2", sessA, "assistant", "好的，已记录会话A的实例 uhost-AAA", "ok")
	seedMsg("m-a-pending", sessA, "assistant", "PENDING-DRAFT-AAA-should-be-dropped", "pending")
	seedMsg("m-b-1", sessB, "user", "这个会话讨论 uhost-BBB", "ok")
	seedMsg("m-b-2", sessB, "assistant", "好的，已记录会话B的实例 uhost-BBB", "ok")

	// Real pool, Capacity 1: leasing a second session evicts the first, so the
	// third lease (A again) must re-rehydrate from the DB rather than reuse a
	// cached engine.
	cfg := &config.Config{Agent: config.AgentConfig{
		LLM: config.LLMConfig{BaseURL: "http://localhost:1", Model: "test-model"},
	}}
	pool := agentpool.New(cfg, messageStore, agentpool.Options{Capacity: 1, IdleTTL: time.Minute})
	defer pool.Close()

	// hydrate leases a session and replays prepareChat's hydration sequence,
	// returning the engine and its release closure (caller must release).
	hydrate := func(sessionID string) (*engine.Engine, func()) {
		eng, release, lErr := pool.Lease(ctx, owner, sessionID)
		require.NoError(t, lErr, "lease %s", sessionID)
		sess, gErr := sessionStore.GetByID(ctx, owner, sessionID)
		require.NoError(t, gErr, "get session %s", sessionID)
		eng.ClearSessionState()
		pc, pErr := engine.ParsePersistedContext(sess.Context)
		require.NoError(t, pErr, "parse context %s", sessionID)
		eng.SetSessionState(pc.AgentSessionState, sess.ContextVersion)
		return eng, release
	}

	historyContains := func(eng *engine.Engine, sub string) bool {
		for _, m := range eng.MessagesSnapshot() {
			if strings.Contains(m.Content, sub) {
				return true
			}
		}
		return false
	}

	// assertSession bundles the per-session correctness contract: own transcript
	// present, other session's transcript absent, pending draft dropped, and the
	// correct SelectedInstanceID hydrated.
	assertSession := func(t *testing.T, eng *engine.Engine, ownMarker, otherMarker, wantInstance string) {
		t.Helper()
		assert.True(t, historyContains(eng, ownMarker), "must carry its own transcript marker %q", ownMarker)
		assert.False(t, historyContains(eng, otherMarker), "must NOT carry the other session's marker %q", otherMarker)
		assert.False(t, historyContains(eng, "PENDING-DRAFT"), "non-ok draft messages must be filtered out of history")
		state, _, hydrated := eng.SessionStateSnapshot()
		assert.True(t, hydrated, "SessionState must be hydrated")
		assert.Equal(t, wantInstance, state.SelectedInstanceID, "must carry the correct per-session SelectedInstanceID")
	}

	// Turn 1 — session A.
	engA1, releaseA1 := hydrate(sessA)
	assertSession(t, engA1, "uhost-AAA", "uhost-BBB", "uhost-AAA")
	releaseA1()

	// Turn 2 — switch to session B (Capacity 1 evicts A).
	engB, releaseB := hydrate(sessB)
	assertSession(t, engB, "uhost-BBB", "uhost-AAA", "uhost-BBB")
	releaseB()

	// Turn 3 — switch back to session A. With A evicted, this is a fresh engine
	// re-rehydrated from the DB; it must carry A's context again with no bleed
	// from B.
	engA2, releaseA2 := hydrate(sessA)
	defer releaseA2()
	assertSession(t, engA2, "uhost-AAA", "uhost-BBB", "uhost-AAA")
	assert.True(t, engA1 != engA2,
		"after eviction, switching back must build a NEW engine re-rehydrated from the DB (not a shared cached one)")
}
