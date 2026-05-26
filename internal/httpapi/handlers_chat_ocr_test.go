package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
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
// OCR mock
// ---------------------------------------------------------------------------

type mockOCR struct {
	text string
	err  error
	seen string // captures the data URL passed to Recognize
}

func (m *mockOCR) Recognize(_ context.Context, imageDataURL string) (string, error) {
	m.seen = imageDataURL
	return m.text, m.err
}

func ocrTestConfig() *config.Config {
	return &config.Config{Agent: config.AgentConfig{
		LLM:  config.LLMConfig{Model: "model-x"},
		HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: time.Hour},
		Meta: config.MetaConfig{MaxInputLength: 4000},
		STS:  config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
		OCR:  config.OCRConfig{Timeout: 10 * time.Second, MaxBytes: 10 * 1024 * 1024},
	}}
}

func ocrTestSession() *mockSessions {
	return &mockSessions{byID: map[string]store.Session{
		"sess-ocr": {
			ID:                "sess-ocr",
			TopOrganizationID: 1,
			OrganizationID:    2,
			CreatedAt:         time.Now(),
			UpdatedAt:         time.Now(),
		},
	}}
}

func makeTestDataURL(payload []byte) string {
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(payload)
}

func dispatchOCR(t *testing.T, h *Handlers, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Dispatch(c)
	return rec
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestChat_OCRTextInjectedViaImageContext(t *testing.T) {
	eng := engine.NewWithDeps(chatLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)

	messages := &recordingMessages{}
	h := NewHandlers(ocrTestConfig(), ocrTestSession(), messages, mockFeedback{}, fakePool{eng: eng}, nil)
	h.SetOCRClient(&mockOCR{text: "nvidia-smi output"})

	imgURL := makeTestDataURL([]byte("fake-img"))
	body := `{"Action":"SendCSAgentChat","SessionId":"sess-ocr","Message":"看看这个","Image":"` + imgURL + `","top_organization_id":1,"organization_id":2}`

	rec := dispatchOCR(t, h, body)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "event: done")

	// DB persisted message should contain structured caption prefix.
	require.True(t, len(messages.appended) >= 1, "expected at least user row")
	userContent := messages.appended[0].Content
	assert.Contains(t, userContent, "用户上传了一张截图，系统自动识别到以下内容")
	assert.Contains(t, userContent, "nvidia-smi output")
	assert.Contains(t, userContent, "看看这个")
}

func TestChat_InvalidImageReturns400(t *testing.T) {
	eng := engine.NewWithDeps(chatLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)

	h := NewHandlers(ocrTestConfig(), ocrTestSession(), &recordingMessages{}, mockFeedback{}, fakePool{eng: eng}, nil)
	h.SetOCRClient(&mockOCR{text: "should not be called"})

	body := `{"Action":"SendCSAgentChat","SessionId":"sess-ocr","Message":"看图","Image":"data:image/jpeg;base64,NOT_VALID!!!","top_organization_id":1,"organization_id":2}`

	rec := dispatchOCR(t, h, body)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "invalid Image")
}

func TestChat_OCRFailureDoesNotBlockChat(t *testing.T) {
	eng := engine.NewWithDeps(chatLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)

	messages := &recordingMessages{}
	h := NewHandlers(ocrTestConfig(), ocrTestSession(), messages, mockFeedback{}, fakePool{eng: eng}, nil)
	h.SetOCRClient(&mockOCR{err: errors.New("model timeout")})

	imgURL := makeTestDataURL([]byte("fake-img"))
	body := `{"Action":"SendCSAgentChat","SessionId":"sess-ocr","Message":"帮我看","Image":"` + imgURL + `","top_organization_id":1,"organization_id":2}`

	rec := dispatchOCR(t, h, body)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "event: done")

	// DB message should NOT contain OCR prefix (OCR failed).
	require.True(t, len(messages.appended) >= 1)
	assert.NotContains(t, messages.appended[0].Content, "系统自动识别到以下内容")
	assert.Contains(t, messages.appended[0].Content, "帮我看")
}

func TestChat_NoOCRClientIgnoresImage(t *testing.T) {
	eng := engine.NewWithDeps(chatLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)

	messages := &recordingMessages{}
	h := NewHandlers(ocrTestConfig(), ocrTestSession(), messages, mockFeedback{}, fakePool{eng: eng}, nil)
	// No SetOCRClient — ocrClient is nil.

	imgURL := makeTestDataURL([]byte("fake-img"))
	body := `{"Action":"SendCSAgentChat","SessionId":"sess-ocr","Message":"看图","Image":"` + imgURL + `","top_organization_id":1,"organization_id":2}`

	rec := dispatchOCR(t, h, body)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "event: done")
	assert.NotContains(t, messages.appended[0].Content, "系统自动识别到以下内容")
}

func TestChat_OCRKeywordsDoNotTriggerPreBlock(t *testing.T) {
	eng := engine.NewWithDeps(chatLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)

	messages := &recordingMessages{}
	h := NewHandlers(ocrTestConfig(), ocrTestSession(), messages, mockFeedback{}, fakePool{eng: eng}, nil)
	// OCR returns text that would trigger monitor-history hard-block
	// if injected verbatim into the user message (contains both "监控" and "最近").
	h.SetOCRClient(&mockOCR{text: "页面/场景：运维监控概览\n可见文字：最近访问、昨天CPU利用率"})

	imgURL := makeTestDataURL([]byte("fake-img"))
	body := `{"Action":"SendCSAgentChat","SessionId":"sess-ocr","Message":"帮我看看","Image":"` + imgURL + `","top_organization_id":1,"organization_id":2}`

	rec := dispatchOCR(t, h, body)

	require.Equal(t, http.StatusOK, rec.Code)
	respBody := rec.Body.String()
	// Must NOT be the monitor-history canned refusal — the user's actual
	// question "帮我看看" contains no monitor/time keywords.
	assert.NotContains(t, respBody, "暂不支持指定历史时间段")
	assert.Contains(t, respBody, "event: token", "should reach LLM, not be hard-blocked")
	assert.Contains(t, respBody, "event: done")
}
