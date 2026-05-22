package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

type mockSessions struct{ byID map[string]store.Session }

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
				MaxInputLength:   4000,
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
	assert.Contains(t, rec.Body.String(), `"Code":"Success"`)
	assert.Contains(t, rec.Body.String(), `"Welcome":"welcome"`)
}

func TestDispatchCreateSession(t *testing.T) {
	h := newTestHandlers()
	rec := performGateway(h, `{"Action":"CreateCSAgentSession","Title":"hello","top_organization_id":1,"organization_id":2}`)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"SessionId":"sess-new"`)
}

func TestDispatchGetSessionRequiresSessionID(t *testing.T) {
	h := newTestHandlers()
	rec := performGateway(h, `{"Action":"GetCSAgentSession","top_organization_id":1,"organization_id":2}`)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), `"Code":"InvalidParam"`)
}

func TestDispatchFeedback(t *testing.T) {
	h := newTestHandlers()
	rec := performGateway(h, `{"Action":"SendCSAgentFeedback","MessageId":"msg-1","Rating":"Up","top_organization_id":1,"organization_id":2}`)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"FeedbackId":"fb-1"`)
}
