package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Local fakes used only in chat tests
// ---------------------------------------------------------------------------

// chatLLM is a mock LLM that streams two deltas ("你" / "好") then returns "你好".
type chatLLM struct{}

func (chatLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	if req.OnTextDelta != nil {
		req.OnTextDelta("你")
		req.OnTextDelta("好")
	}
	return &llm.ChatResponse{
		Content: "你好",
		Usage:   llm.TokenUsage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
	}, nil
}

// chatExecutor satisfies tools.ToolExecutor so engine.NewWithDeps compiles.
type chatExecutor struct{}

func (chatExecutor) Execute(_ context.Context, _ string, _ map[string]any) (map[string]any, error) {
	return map[string]any{}, nil
}

// fakePool implements EnginePool and always returns the same engine.
type fakePool struct{ eng *engine.Engine }

func (p fakePool) Lease(_ context.Context, _ store.Owner, _ string) (*engine.Engine, func(), error) {
	return p.eng, func() {}, nil
}
func (p fakePool) Get(_ context.Context, _ store.Owner, _ string) (*engine.Engine, error) {
	return p.eng, nil
}

// recordingMessages extends mockMessages to record Append and UpdateAssistant calls.
type recordingMessages struct {
	mockMessages
	appended []store.Message
	patch    store.AssistantPatch
}

func (m *recordingMessages) Append(_ context.Context, msg store.Message) error {
	m.appended = append(m.appended, msg)
	return nil
}

func (m *recordingMessages) UpdateAssistant(_ context.Context, _ store.Owner, _ string, patch store.AssistantPatch) error {
	m.patch = patch
	return nil
}

// denyConfirm is the confirm callback used in tests — always denies (unused in happy path).
func denyConfirm(_ string, _ map[string]any) bool { return false }

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestDispatchChatStreamsMetaTokenDone(t *testing.T) {
	// Build an engine backed by the streaming LLM mock.
	eng := engine.NewWithDeps(chatLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)

	messages := &recordingMessages{}
	h := NewHandlers(
		&config.Config{Agent: config.AgentConfig{
			LLM:  config.LLMConfig{Model: "model-x"},
			HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: time.Hour},
			Meta: config.MetaConfig{MaxInputLength: 4000},
			STS:  config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
		}},
		&mockSessions{byID: map[string]store.Session{
			"sess-1": {
				ID:                "sess-1",
				TopOrganizationID: 1,
				OrganizationID:    2,
				CreatedAt:         time.Now(),
				UpdatedAt:         time.Now(),
			},
		}},
		messages,
		mockFeedback{},
		fakePool{eng: eng},
	)

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/gateway",
		strings.NewReader(`{"Action":"Chat","SessionId":"sess-1","Message":"hi","request_uuid":"req-1","top_organization_id":1,"organization_id":2}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.Dispatch(c)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "event: meta")
	assert.Contains(t, body, `"RequestId":"req-1"`)
	assert.Contains(t, body, "event: token")
	assert.Contains(t, body, `"Text":"你"`)
	assert.Contains(t, body, `"Text":"好"`)
	assert.Contains(t, body, "event: done")
	require.Len(t, messages.appended, 2, "expected user and assistant rows inserted")
	assert.Equal(t, "user", messages.appended[0].Role)
	assert.Equal(t, "assistant", messages.appended[1].Role)
	assert.Equal(t, "你好", messages.patch.Content)
	assert.Equal(t, "ok", messages.patch.Status)
}
