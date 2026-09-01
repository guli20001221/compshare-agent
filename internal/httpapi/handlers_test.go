package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Mock stores
// ---------------------------------------------------------------------------

type mockSessions struct {
	byID map[string]store.Session
	// updateContextCalls counts every UpdateContext invocation regardless
	// of outcome. Tests use this to assert "no persistence on parse
	// failure" and "exactly one persist on happy path".
	updateContextCalls int
	// lastUpdateContext records the most recent UpdateContext payload for
	// assertions about envelope shape / version.
	lastUpdateContext struct {
		sessionID       string
		ctxJSON         json.RawMessage
		expectedVersion int
	}
	// updateContextOverride, when non-nil, replaces the default CAS
	// behavior. Tests use it to force outcomes like ErrStaleWrite.
	updateContextOverride func(sessionID string, ctxJSON json.RawMessage, expectedVersion int) (int, error)
	// setTitleCallCount counts SetTitleIfEmpty invocations so tests can assert
	// the chat write path attempts first-turn title derivation.
	setTitleCallCount int
	// lastListLimit records the limit ListByOwner was last called with so tests
	// can assert the handler's clamp logic.
	lastListLimit int
}

func (m *mockSessions) Create(_ context.Context, owner store.Owner, title *string, ctxJSON json.RawMessage) (store.Session, error) {
	s := store.Session{
		ID:                "sess-new",
		TopOrganizationID: owner.TopOrganizationID,
		OrganizationID:    owner.OrganizationID,
		Title:             title,
		Context:           ctxJSON,
		CreatedAt:         time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC),
		UpdatedAt:         time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC),
	}
	if m.byID == nil {
		m.byID = map[string]store.Session{}
	}
	m.byID[s.ID] = s
	return s, nil
}

func (m *mockSessions) GetByID(_ context.Context, owner store.Owner, sessionID string) (store.Session, error) {
	s, ok := m.byID[sessionID]
	if !ok || s.TopOrganizationID != owner.TopOrganizationID || s.OrganizationID != owner.OrganizationID {
		return store.Session{}, sql.ErrNoRows
	}
	return s, nil
}

func (m *mockSessions) BumpUpdatedAtAndIncCount(_ context.Context, _ store.Owner, _ string, _ int) error {
	return nil
}

// UpdateContext mimics the CAS semantics of MySQLSessionStore.UpdateContext
// against the in-memory byID map: version match → write + increment, return
// new version; mismatch (or missing row / wrong owner) → ErrStaleWrite.
//
// Tests inject updateContextOverride to force specific outcomes (e.g.
// ErrStaleWrite even when versions match). All paths increment
// updateContextCalls so "was UpdateContext called at all?" assertions
// work regardless of override.
func (m *mockSessions) UpdateContext(_ context.Context, owner store.Owner, sessionID string, ctxJSON json.RawMessage, expectedVersion int) (int, error) {
	m.updateContextCalls++
	m.lastUpdateContext.sessionID = sessionID
	m.lastUpdateContext.ctxJSON = append(json.RawMessage(nil), ctxJSON...)
	m.lastUpdateContext.expectedVersion = expectedVersion
	if m.updateContextOverride != nil {
		return m.updateContextOverride(sessionID, ctxJSON, expectedVersion)
	}
	s, ok := m.byID[sessionID]
	if !ok || s.TopOrganizationID != owner.TopOrganizationID || s.OrganizationID != owner.OrganizationID {
		return 0, store.ErrStaleWrite
	}
	if s.ContextVersion != expectedVersion {
		return 0, store.ErrStaleWrite
	}
	s.Context = ctxJSON
	s.ContextVersion = expectedVersion + 1
	m.byID[sessionID] = s
	return s.ContextVersion, nil
}

// ListByOwner mirrors MySQLSessionStore.ListByOwner: owner-scoped, most
// recently active first (updated_at DESC), capped at limit. The in-memory
// mock has no deleted_at, so all owned rows are eligible.
func (m *mockSessions) ListByOwner(_ context.Context, owner store.Owner, limit int) ([]store.Session, error) {
	m.lastListLimit = limit
	if limit < 1 {
		return []store.Session{}, nil
	}
	out := make([]store.Session, 0, len(m.byID))
	for _, s := range m.byID {
		if s.TopOrganizationID == owner.TopOrganizationID && s.OrganizationID == owner.OrganizationID {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if limit < len(out) {
		out = out[:limit]
	}
	return out, nil
}

// SetTitleIfEmpty mirrors the store: sets title only when currently nil,
// owner-scoped. 0-effect (title already set / missing row) is not an error.
func (m *mockSessions) SetTitleIfEmpty(_ context.Context, owner store.Owner, sessionID, title string) error {
	m.setTitleCallCount++
	s, ok := m.byID[sessionID]
	if ok && s.Title == nil && s.TopOrganizationID == owner.TopOrganizationID && s.OrganizationID == owner.OrganizationID {
		t := title
		s.Title = &t
		m.byID[sessionID] = s
	}
	return nil
}

type mockMessages struct {
	list    []store.Message
	checked map[string]store.Message
}

func (m *mockMessages) Append(_ context.Context, _ store.Message) error { return nil }
func (m *mockMessages) UpdateAssistant(_ context.Context, _ store.Owner, _ string, _ store.AssistantPatch) error {
	return nil
}
func (m *mockMessages) ListBySession(_ context.Context, _ string, _ int, _ string) ([]store.Message, string, error) {
	return m.list, "", nil
}
func (m *mockMessages) GetWithOwnerCheck(_ context.Context, _ store.Owner, msgID string) (store.Message, error) {
	msg, ok := m.checked[msgID]
	if !ok {
		return store.Message{}, sql.ErrNoRows
	}
	return msg, nil
}

type mockFeedback struct{}

func (mockFeedback) Insert(_ context.Context, _, _, _ string) (string, error) { return "fb-1", nil }

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newTestHandlers() *Handlers {
	title := "session title"
	return NewHandlers(
		&config.Config{Agent: config.AgentConfig{
			LLM: config.LLMConfig{Model: "model-x"},
			Meta: config.MetaConfig{
				Welcome:          "welcome",
				SuggestedPrompts: []string{"p1"},
			},
			HTTP: config.HTTPConfig{
				MaxInputLength:       4000,
				SSEKeepaliveInterval: 15 * time.Second,
			},
		}},
		&mockSessions{byID: map[string]store.Session{
			"sess-1": {
				ID:                "sess-1",
				TopOrganizationID: 1,
				OrganizationID:    2,
				Title:             &title,
				CreatedAt:         time.Now(),
				UpdatedAt:         time.Now(),
			},
		}},
		&mockMessages{checked: map[string]store.Message{
			"msg-1": {ID: "msg-1", SessionID: "sess-1", Role: "assistant", Status: "ok"},
		}},
		mockFeedback{},
		nil,
		nil,
	)
}

func performGateway(h *Handlers, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/gateway", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Dispatch(c)
	return rec
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestDispatchGetMeta(t *testing.T) {
	h := newTestHandlers()
	rec := performGateway(h, `{"Action":"GetCSAgentMeta","top_organization_id":1,"organization_id":2}`)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"RetCode":0`)
	assert.Contains(t, rec.Body.String(), `"Welcome":"welcome"`)
}

func TestDispatchCreateSession(t *testing.T) {
	h := newTestHandlers()
	rec := performGateway(h, `{"Action":"CreateCSAgentSession","Title":"hello","top_organization_id":1,"organization_id":2}`)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"SessionId":"sess-new"`)
}

func TestDispatchCreateSessionRedactsCallerSuppliedAuthorizationTitle(t *testing.T) {
	const secret = "create-title-secret-0123456789"
	sessions := &mockSessions{}
	h := newListTestHandlers(sessions)
	rec := performGateway(h, `{"Action":"CreateCSAgentSession","Title":"Authorization: Bearer `+secret+`","top_organization_id":1,"organization_id":2}`)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), secret)
	stored := sessions.byID["sess-new"]
	require.NotNil(t, stored.Title)
	assert.NotContains(t, *stored.Title, secret)
	assert.Contains(t, *stored.Title, "Authorization")
}

func TestDispatchGetSessionRedactsHistoricalAuthorizationTitle(t *testing.T) {
	const secret = "historical-get-title-secret-0123456789"
	title := "Authorization: Bearer " + secret
	sessions := &mockSessions{byID: map[string]store.Session{
		"sess-history": {
			ID: "sess-history", TopOrganizationID: 1, OrganizationID: 2,
			Title: &title, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		},
	}}
	h := newListTestHandlers(sessions)
	rec := performGateway(h, `{"Action":"GetCSAgentSession","SessionId":"sess-history","top_organization_id":1,"organization_id":2}`)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), secret)
	assert.Contains(t, rec.Body.String(), "Authorization")
}

func TestDispatchGetSessionRequiresSessionID(t *testing.T) {
	h := newTestHandlers()
	rec := performGateway(h, `{"Action":"GetCSAgentSession","top_organization_id":1,"organization_id":2}`)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), `"Code":"InvalidParam"`)
	assert.Contains(t, rec.Body.String(), `"RetCode":226612`)
}

func TestWriteErrorFlattensStableCodeIntoTopLevelEnvelope(t *testing.T) {
	h := newTestHandlers()
	rec := performGateway(h, `{"Action":"UnknownAction","request_uuid":"req-code","top_organization_id":1,"organization_id":2}`)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "InvalidParam", body["Code"])
	assert.EqualValues(t, ErrInvalidParam.RetCode, body["RetCode"])
	assert.Equal(t, "UnknownAction", body["Action"])
	assert.Equal(t, "req-code", body["RequestId"])
	_, nested := body["Data"]
	assert.False(t, nested, "the stable error code belongs to the existing flat envelope")
}

func TestDispatchGetSessionMissingSessionDoesNotCreateReplacement(t *testing.T) {
	h := newTestHandlers()
	rec := performGateway(h, `{"Action":"GetCSAgentSession","SessionId":"stale-session","top_organization_id":1,"organization_id":2}`)

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), `"RetCode":226615`)
	assert.NotContains(t, rec.Body.String(), `"SessionId":"sess-new"`)
}

func TestDispatchFeedback(t *testing.T) {
	h := newTestHandlers()
	rec := performGateway(h, `{"Action":"SendCSAgentFeedback","MessageId":"msg-1","Rating":"Up","top_organization_id":1,"organization_id":2}`)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"FeedbackId":"fb-1"`)
}
