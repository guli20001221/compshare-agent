package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/agentpool"
	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/guardrails"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/refusal"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
	"github.com/gin-gonic/gin"
	openai "github.com/sashabaranov/go-openai"
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

type rehydratingMessages struct {
	recordingMessages
	list []store.Message
}

func (m *rehydratingMessages) Append(_ context.Context, msg store.Message) error {
	m.appended = append(m.appended, msg)
	m.list = append(m.list, msg)
	return nil
}

func (m *rehydratingMessages) ListBySession(_ context.Context, _ string, _ int, _ string) ([]store.Message, string, error) {
	return append([]store.Message(nil), m.list...), "", nil
}

type captureLLM struct {
	messages []openai.ChatCompletionMessage
}

func (c *captureLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	c.messages = append([]openai.ChatCompletionMessage(nil), req.Messages...)
	if req.OnTextDelta != nil {
		req.OnTextDelta("ok")
	}
	return &llm.ChatResponse{Content: "ok"}, nil
}

// scriptedChatLLM returns a fixed response and captures the model request.
type scriptedChatLLM struct {
	content  string
	messages []openai.ChatCompletionMessage
}

func (c *scriptedChatLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	c.messages = append([]openai.ChatCompletionMessage(nil), req.Messages...)
	if req.OnTextDelta != nil {
		req.OnTextDelta(c.content)
	}
	return &llm.ChatResponse{
		Content: c.content,
		Usage:   llm.TokenUsage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
	}, nil
}

type streamingErrorLLM struct {
	token string
}

func (c streamingErrorLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	if req.OnTextDelta != nil {
		req.OnTextDelta(c.token)
	}
	return nil, errors.New("llm failed")
}

// denyConfirm is the confirm callback used in tests.
func denyConfirm(_ string, _ map[string]any) bool { return false }

type captureTraceWriter struct {
	records []observability.TraceRecord
	tenants []observability.TenantContext
}

func (w *captureTraceWriter) Append(record observability.TraceRecord) error {
	w.records = append(w.records, record)
	w.tenants = append(w.tenants, observability.TenantContext{})
	return nil
}

func (w *captureTraceWriter) Enqueue(tenant observability.TenantContext, record observability.TraceRecord) error {
	w.records = append(w.records, record)
	w.tenants = append(w.tenants, tenant)
	return nil
}

func (w *captureTraceWriter) Dir() string { return "" }

func (w *captureTraceWriter) Close(context.Context) error { return nil }

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
		nil,
	)

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/gateway",
		strings.NewReader(`{"Action":"SendCSAgentChat","SessionId":"sess-1","Message":"hi","request_uuid":"req-1","top_organization_id":1,"organization_id":2}`),
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

func TestDispatchChatWritesTraceWithTenantAndSession(t *testing.T) {
	eng := engine.NewWithDeps(chatLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)

	traceWriter := &captureTraceWriter{}
	messages := &recordingMessages{}
	h := NewHandlers(
		&config.Config{Agent: config.AgentConfig{
			LLM:  config.LLMConfig{Model: "model-x"},
			HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: time.Hour},
			Meta: config.MetaConfig{MaxInputLength: 4000},
			STS:  config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
		}},
		&mockSessions{byID: map[string]store.Session{
			"sess-trace": {
				ID:                "sess-trace",
				TopOrganizationID: 7,
				OrganizationID:    8,
				CreatedAt:         time.Now(),
				UpdatedAt:         time.Now(),
			},
		}},
		messages,
		mockFeedback{},
		fakePool{eng: eng},
		traceWriter,
	)

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/",
		strings.NewReader(`{"Action":"Chat","SessionId":"sess-trace","Message":"hi","request_uuid":"req-trace","top_organization_id":7,"organization_id":8}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.Dispatch(c)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, traceWriter.records, 1)
	require.Len(t, traceWriter.tenants, 1)
	trace := traceWriter.records[0]
	tenant := traceWriter.tenants[0]
	assert.Equal(t, "req-trace", trace.TraceID)
	assert.Equal(t, "turn-1", trace.TurnID)
	assert.Equal(t, 1, trace.TurnIndex)
	assert.NotEmpty(t, trace.UserMsgHash)
	assert.Equal(t, 3, trace.Outcome.TotalTokens)
	assert.GreaterOrEqual(t, trace.Outcome.TotalLatencyMS, int64(0))
	assert.Equal(t, int64(7), tenant.TopOrgID)
	assert.Equal(t, int64(8), tenant.OrgID)
	assert.Equal(t, "sess-trace", tenant.ConnectionID)
}

func TestDispatchChatRedactsUserPIIOnlyWhenPersisting(t *testing.T) {
	llmClient := &scriptedChatLLM{content: "ok"}
	eng := engine.NewWithDeps(llmClient, tools.ToolExecutor(chatExecutor{}), denyConfirm)
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
			"sess-pii": {
				ID:                "sess-pii",
				TopOrganizationID: 1,
				OrganizationID:    2,
				CreatedAt:         time.Now(),
				UpdatedAt:         time.Now(),
			},
		}},
		messages,
		mockFeedback{},
		fakePool{eng: eng},
		nil,
	)

	const userMessage = "phone 13800138000 email user@example.com instance uhost-abc123 wants 4090 price"
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/",
		strings.NewReader(`{"Action":"Chat","SessionId":"sess-pii","Message":"`+userMessage+`","request_uuid":"req-pii","top_organization_id":1,"organization_id":2}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.Dispatch(c)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, messages.appended, 2)
	persisted := messages.appended[0].Content
	assert.Contains(t, persisted, guardrails.PhoneRedacted)
	assert.Contains(t, persisted, guardrails.EmailRedacted)
	assert.NotContains(t, persisted, "13800138000")
	assert.NotContains(t, persisted, "user@example.com")
	assert.Contains(t, persisted, "uhost-abc123")
	assert.Contains(t, persisted, "4090")

	require.NotEmpty(t, llmClient.messages)
	rawUserSeen := false
	for _, msg := range llmClient.messages {
		if msg.Role == openai.ChatMessageRoleUser && msg.Content == userMessage {
			rawUserSeen = true
		}
	}
	assert.True(t, rawUserSeen, "agent routing/model input must still see raw user text")
}

func TestDispatchChatRedactsAssistantLeakOnlyWhenPersisting(t *testing.T) {
	reply := `Instance uhost-abc123 is ready on 4090.
Public IP: 1.2.3.4
Project: 12345678-1234-1234-1234-1234567890ab
AccessKey="AKIAIOSFODNN7EXAMPLE"
token=AKIAIOSFODNN7EXAMPLEbCDEF`
	llmClient := &scriptedChatLLM{content: reply}
	eng := engine.NewWithDeps(llmClient, tools.ToolExecutor(chatExecutor{}), denyConfirm)
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
			"sess-output": {
				ID:                "sess-output",
				TopOrganizationID: 1,
				OrganizationID:    2,
				CreatedAt:         time.Now(),
				UpdatedAt:         time.Now(),
			},
		}},
		messages,
		mockFeedback{},
		fakePool{eng: eng},
		nil,
	)

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/",
		strings.NewReader(`{"Action":"Chat","SessionId":"sess-output","Message":"hi","request_uuid":"req-output","top_organization_id":1,"organization_id":2}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.Dispatch(c)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "event: token")
	assert.Contains(t, body, "1.2.3.4")
	assert.Contains(t, body, "12345678-1234-1234-1234-1234567890ab")
	assert.Contains(t, body, "AKIAIOSFODNN7EXAMPLE")
	assert.NotContains(t, body, guardrails.IPRedacted)
	assert.NotContains(t, body, guardrails.ProjectIDRedacted)
	assert.NotContains(t, body, guardrails.CredentialRedactedOutput)
	assert.NotContains(t, body, guardrails.TokenRedactedOutput)

	persisted := messages.patch.Content
	assert.Contains(t, persisted, guardrails.IPRedacted)
	assert.Contains(t, persisted, guardrails.ProjectIDRedacted)
	assert.Contains(t, persisted, guardrails.CredentialRedactedOutput)
	assert.Contains(t, persisted, guardrails.TokenRedactedOutput)
	assert.NotContains(t, persisted, "1.2.3.4")
	assert.NotContains(t, persisted, "12345678-1234-1234-1234-1234567890ab")
	assert.NotContains(t, persisted, "AKIAIOSFODNN7EXAMPLE")
	assert.Contains(t, persisted, "uhost-abc123")
	assert.Contains(t, persisted, "4090")
}

func TestDispatchChatDoesNotPersistPartialAssistantContentOnError(t *testing.T) {
	const leakedDelta = `partial reply Public IP: 1.2.3.4 token=AKIAIOSFODNN7EXAMPLEbCDEF`
	eng := engine.NewWithDeps(streamingErrorLLM{token: leakedDelta}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
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
			"sess-error": {
				ID:                "sess-error",
				TopOrganizationID: 1,
				OrganizationID:    2,
				CreatedAt:         time.Now(),
				UpdatedAt:         time.Now(),
			},
		}},
		messages,
		mockFeedback{},
		fakePool{eng: eng},
		nil,
	)

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/",
		strings.NewReader(`{"Action":"Chat","SessionId":"sess-error","Message":"hi","request_uuid":"req-error","top_organization_id":1,"organization_id":2}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.Dispatch(c)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, leakedDelta)
	assert.Contains(t, body, "event: error")

	assert.Equal(t, "error", messages.patch.Status)
	assert.Empty(t, messages.patch.Content)
	assert.NotContains(t, messages.patch.Content, "1.2.3.4")
	assert.NotContains(t, messages.patch.Content, "AKIAIOSFODNN7EXAMPLE")
}

func TestDispatchChatEmitsTokenForDirectEngineReply(t *testing.T) {
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
			"sess-direct": {
				ID:                "sess-direct",
				TopOrganizationID: 1,
				OrganizationID:    2,
				CreatedAt:         time.Now(),
				UpdatedAt:         time.Now(),
			},
		}},
		messages,
		mockFeedback{},
		fakePool{eng: eng},
		nil,
	)

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/",
		strings.NewReader(`{"Action":"Chat","SessionId":"sess-direct","Message":"看 2026-04-29 14:00 的监控","request_uuid":"req-direct","top_organization_id":1,"organization_id":2}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.Dispatch(c)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "event: token")
	assert.Contains(t, body, refusal.MonitorHistoryUnsupported)
	assert.Contains(t, body, "event: done")
	assert.Equal(t, refusal.MonitorHistoryUnsupported, messages.patch.Content)
}

func TestDispatchChatColdSessionDoesNotRehydrateCurrentUserMessage(t *testing.T) {
	captured := &captureLLM{}
	messages := &rehydratingMessages{}
	deps := &engine.SharedDeps{
		LLMClient:                captured,
		RateLimiter:              governance.NewMemoryLimiter(governance.DefaultLimits()),
		SupportsObjectToolChoice: true,
		ExternalExecutor:         tools.ToolExecutor(chatExecutor{}),
	}
	pool := agentpool.NewWithDeps(deps, messages, agentpool.Options{
		Capacity: 10,
		IdleTTL:  time.Hour,
	})
	defer pool.Close()

	h := NewHandlers(
		&config.Config{Agent: config.AgentConfig{
			LLM:  config.LLMConfig{Model: "model-x"},
			HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: time.Hour},
			Meta: config.MetaConfig{MaxInputLength: 4000},
			STS:  config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
		}},
		&mockSessions{byID: map[string]store.Session{
			"sess-cold": {
				ID:                "sess-cold",
				TopOrganizationID: 1,
				OrganizationID:    2,
				CreatedAt:         time.Now(),
				UpdatedAt:         time.Now(),
			},
		}},
		messages,
		mockFeedback{},
		pool,
		nil,
	)

	const userMessage = "hello current"
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/",
		strings.NewReader(`{"Action":"Chat","SessionId":"sess-cold","Message":"`+userMessage+`","request_uuid":"req-cold","top_organization_id":1,"organization_id":2}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.Dispatch(c)

	require.Equal(t, http.StatusOK, rec.Code)
	userCopies := 0
	for _, msg := range captured.messages {
		if msg.Role == openai.ChatMessageRoleUser && msg.Content == userMessage {
			userCopies++
		}
	}
	require.Equal(t, 1, userCopies, "current user message must be sent to the model exactly once on cold sessions")
}

func TestDispatchChatRejectsWhenSessionTurnLimitReached(t *testing.T) {
	eng := engine.NewWithDeps(chatLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)

	h := NewHandlers(
		&config.Config{Agent: config.AgentConfig{
			LLM:  config.LLMConfig{Model: "model-x"},
			HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: time.Hour, MaxSessionTurns: 3},
			Meta: config.MetaConfig{MaxInputLength: 4000},
			STS:  config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
		}},
		&mockSessions{byID: map[string]store.Session{
			"sess-cap": {
				ID:                "sess-cap",
				TopOrganizationID: 1,
				OrganizationID:    2,
				MessageCount:      6, // 3 user+assistant pairs = at cap
				CreatedAt:         time.Now(),
				UpdatedAt:         time.Now(),
			},
		}},
		&recordingMessages{},
		mockFeedback{},
		fakePool{eng: eng},
		nil,
	)

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/",
		strings.NewReader(`{"Action":"SendCSAgentChat","SessionId":"sess-cap","Message":"hi","request_uuid":"req-cap","top_organization_id":1,"organization_id":2}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.Dispatch(c)

	require.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), `"Code":"SessionTurnLimitExceeded"`)
}
