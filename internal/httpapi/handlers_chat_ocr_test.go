package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
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

func dispatchOCR(t *testing.T, h *Handlers, body string) (*recordingSink, *APIError) {
	t.Helper()
	return runChatJSON(t, h, body)
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

	sink, _ := dispatchOCR(t, h, body)

	assert.True(t, sink.has("done"))

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

	sink, apiErr := dispatchOCR(t, h, body)
	require.NotNil(t, apiErr, "invalid image must fail before streaming")
	assert.Equal(t, http.StatusBadRequest, apiErr.Status)
	assert.Equal(t, ErrInvalidParam.RetCode, apiErr.RetCode)
	assert.Contains(t, apiErr.Message, "invalid Image")
	assert.Empty(t, sink.events, "no frames should stream when validation fails pre-stream")
}

func TestChat_OCRFailureDoesNotBlockChat(t *testing.T) {
	eng := engine.NewWithDeps(chatLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)

	messages := &recordingMessages{}
	h := NewHandlers(ocrTestConfig(), ocrTestSession(), messages, mockFeedback{}, fakePool{eng: eng}, nil)
	h.SetOCRClient(&mockOCR{err: errors.New("model timeout")})

	imgURL := makeTestDataURL([]byte("fake-img"))
	body := `{"Action":"SendCSAgentChat","SessionId":"sess-ocr","Message":"帮我看","Image":"` + imgURL + `","top_organization_id":1,"organization_id":2}`

	sink, _ := dispatchOCR(t, h, body)

	assert.True(t, sink.has("done"))

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

	sink, _ := dispatchOCR(t, h, body)

	assert.True(t, sink.has("done"))
	assert.NotContains(t, messages.appended[0].Content, "系统自动识别到以下内容")
}
