package httpapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
	"github.com/gin-gonic/gin"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

// Two production turns the day after #629 ended with the transport, not the
// work: a client that disconnected while a second create card was open lost the
// row for the instance the first card had already created, and a client that
// silently vanished held the connection open for its whole 54-minute lifetime
// while the finished turn sat unrecorded. The tests here drive the real WS
// handler through both.

// syncedMessages is a message store safe to poll from the test goroutine while
// chatStream persists from its own; patched is signalled on every assistant
// patch.
type syncedMessages struct {
	mockMessages
	mu      sync.Mutex
	patch   store.AssistantPatch
	patched chan struct{}
}

func newSyncedMessages() *syncedMessages {
	return &syncedMessages{patched: make(chan struct{}, 4)}
}

func (m *syncedMessages) UpdateAssistant(_ context.Context, _ store.Owner, _ string, patch store.AssistantPatch) error {
	m.mu.Lock()
	m.patch = patch
	m.mu.Unlock()
	m.patched <- struct{}{}
	return nil
}

func (m *syncedMessages) waitPatch(t *testing.T, within time.Duration) store.AssistantPatch {
	t.Helper()
	select {
	case <-m.patched:
	case <-time.After(within):
		t.Fatal("the assistant row was not persisted in time")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.patch
}

// syncedTraceWriter mirrors captureTraceWriter with a mutex, for the same reason.
type syncedTraceWriter struct {
	mu      sync.Mutex
	records []observability.TraceRecord
}

func (w *syncedTraceWriter) Append(record observability.TraceRecord) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.records = append(w.records, record)
	return nil
}

func (w *syncedTraceWriter) Enqueue(_ observability.TenantContext, record observability.TraceRecord) error {
	return w.Append(record)
}

func (w *syncedTraceWriter) Dir() string { return "" }

func (w *syncedTraceWriter) Close(context.Context) error { return nil }

func (w *syncedTraceWriter) last(t *testing.T) observability.TraceRecord {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	require.NotEmpty(t, w.records, "the turn must have written its trace")
	return w.records[len(w.records)-1]
}

func wsServerFor(t *testing.T, eng *engine.Engine, keepalive time.Duration, messages store.MessageStore) (*httptest.Server, *Handlers) {
	t.Helper()
	h := NewHandlers(
		&config.Config{Agent: config.AgentConfig{
			LLM:  config.LLMConfig{Model: "model-x"},
			HTTP: config.HTTPConfig{MaxInputLength: 4000, SSEKeepaliveInterval: keepalive},
			STS:  config.STSConfig{RoleUrnTemplate: "ucs:iam::%d:role/test"},
		}},
		&mockSessions{byID: map[string]store.Session{"sess-1": {
			ID: "sess-1", TopOrganizationID: 1, OrganizationID: 2, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}}},
		messages,
		mockFeedback{},
		fakePool{eng: eng},
		nil,
	)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/", h.HandleWS)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv, h
}

// echoingStartExecutor answers every existence check with the exact instance
// that was asked for, Stopped, so any number of start proposals in one batch
// reach their own card; the start itself is accepted.
type echoingStartExecutor struct{}

func (echoingStartExecutor) Execute(_ context.Context, action string, args map[string]any) (map[string]any, error) {
	switch action {
	case "DescribeCompShareInstance":
		id := "uhost-1"
		switch ids := args["UHostIds"].(type) {
		case []string:
			if len(ids) > 0 {
				id = ids[0]
			}
		case []any:
			if len(ids) > 0 {
				id, _ = ids[0].(string)
			}
		}
		if single, ok := args["UHostIds.0"].(string); ok && single != "" {
			id = single
		}
		return map[string]any{"UHostSet": []any{map[string]any{
			"UHostId": id, "Name": "host-" + id, "State": "Stopped",
			"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "ChargeType": "Postpay", "GpuType": "4090",
		}}}, nil
	case "DescribeCompShareSupportZone":
		return map[string]any{"ZoneInfo": []any{map[string]any{
			"Zone": "cn-wlcb-01", "Region": "cn-wlcb", "RegionId": float64(3001), "ZoneId": float64(10027), "IsPod": false,
		}}}, nil
	default:
		return map[string]any{"RetCode": float64(0)}, nil
	}
}

// twoStartsLLM proposes two starts in one round, the shape of the production
// turn that lost its first create; anything after that is plain text.
type twoStartsLLM struct {
	mu    sync.Mutex
	calls int
}

func (m *twoStartsLLM) Chat(_ context.Context, _ llm.ChatRequest) (*llm.ChatResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.calls == 1 {
		return &llm.ChatResponse{ToolCalls: []openai.ToolCall{
			{ID: "call-1", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "RequestStartInstance", Arguments: `{"UHostId":"uhost-1","StartMode":"normal"}`}},
			{ID: "call-2", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "RequestStartInstance", Arguments: `{"UHostId":"uhost-2","StartMode":"normal"}`}},
		}}, nil
	}
	return &llm.ChatResponse{Content: "两台都已处理。"}, nil
}

// blockingLLM never answers: the turn lives until its context ends, the way a
// long tool turn does while the client is waiting.
type blockingLLM struct{}

func (blockingLLM) Chat(ctx context.Context, _ llm.ChatRequest) (*llm.ChatResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func readUntilConfirmation(t *testing.T, ctx context.Context, conn *websocket.Conn) string {
	t.Helper()
	for {
		f := readOneFrame(t, ctx, conn)
		switch f["event"] {
		case "confirmation":
			id, _ := f["ConfirmationId"].(string)
			require.NotEmpty(t, id)
			return id
		case "done", "error":
			t.Fatalf("turn ended before a confirmation frame: %v", f)
		}
	}
}

// The first card is approved and its start commits; the client drops while the
// second card is open. The aborted row must still carry the committed start —
// the instance is running and billing whether or not the reply was delivered.
func TestWS_DisconnectAfterACommittedWriteKeepsTheWriteInTheAbortedRow(t *testing.T) {
	eng := engine.NewWithDeps(&twoStartsLLM{}, tools.ToolExecutor(echoingStartExecutor{}), denyConfirm)
	eng.SetMutatingToolsEnabled(true)
	eng.RehydrateHistory(nil)
	messages := newSyncedMessages()
	srv, h := wsServerFor(t, eng, time.Hour, messages)
	traces := &syncedTraceWriter{}
	h.traceWriter = traces

	conn := dialWS(t, srv, gatewayHeaders())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(
		`{"Action":"SendCSAgentChat","SessionId":"sess-1","Message":"把 uhost-1 和 uhost-2 都开机"}`)))

	first := readUntilConfirmation(t, ctx, conn)
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(
		`{"Action":"ConfirmCSAgentAction","SessionId":"sess-1","ConfirmationId":"`+first+`","Confirmed":true}`)))
	second := readUntilConfirmation(t, ctx, conn)
	require.NotEqual(t, first, second, "the second proposal gets its own card")

	// The user is gone: no decision, no close frame, the socket just ends.
	require.NoError(t, conn.CloseNow())

	patch := messages.waitPatch(t, 5*time.Second)
	require.Equal(t, "aborted", patch.Status)
	require.True(t, strings.HasPrefix(patch.Content, abortedAssistantMessage), patch.Content)
	require.Contains(t, patch.Content, "本轮已完成的操作：")
	require.Contains(t, patch.Content, "uhost-1", "the committed start must be visible in the aborted row")
	require.Contains(t, patch.Content, "执行开机")
	require.NotContains(t, patch.Content, "uhost-2", "the second start never ran and must not be reported")
	require.NotContains(t, patch.Content, "两台都已处理", "a model answer for a departed client is not delivered")
	record := traces.last(t)
	require.Equal(t, observability.TerminatedByUserCancel, record.Outcome.TerminatedBy)
	require.Equal(t, observability.AbortCauseClientDisconnect, record.Outcome.AbortCause)
}

// A peer that stops consuming without closing used to hold the connection for
// its whole lifetime: the keepalive ping waited for a pong under the lifetime
// context while holding the write mutex, so nothing else could be written and
// the turn's work was recorded 54 minutes later as a model timeout. The ping now
// has its own deadline; missing it ends the turn as a client disconnect.
func TestWS_SilentPeerEndsTheTurnAsAClientDisconnectWithinTheKeepaliveDeadline(t *testing.T) {
	eng := engine.NewWithDeps(blockingLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)
	messages := newSyncedMessages()
	srv, h := wsServerFor(t, eng, 50*time.Millisecond, messages)
	traces := &syncedTraceWriter{}
	h.traceWriter = traces

	conn := dialWS(t, srv, gatewayHeaders())
	t.Cleanup(func() { _ = conn.CloseNow() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(
		`{"Action":"SendCSAgentChat","SessionId":"sess-1","Message":"hi"}`)))
	meta := readOneFrame(t, ctx, conn)
	require.Equal(t, "meta", meta["event"], "the turn started")
	// From here the client never reads again, so it never answers a ping — the
	// same as a laptop that went to sleep mid-turn.

	started := time.Now()
	patch := messages.waitPatch(t, 5*time.Second)
	require.Less(t, time.Since(started), 3*time.Second,
		"the turn must end within a few keepalive deadlines, not at the connection lifetime")
	require.Equal(t, "aborted", patch.Status)
	require.Equal(t, abortedAssistantMessage, patch.Content)
	record := traces.last(t)
	require.Equal(t, observability.TerminatedByUserCancel, record.Outcome.TerminatedBy)
	require.Equal(t, observability.AbortCauseClientDisconnect, record.Outcome.AbortCause)
}

// The connection lifetime is a transport backstop. When it expires, the row is
// an aborted delivery like a disconnect — not a model timeout with an empty
// body, which is how the 54-minute production turn was recorded.
func TestChatStreamConnectionLifetimeExpiryIsAnAbortedDeliveryNotAModelTimeout(t *testing.T) {
	eng := engine.NewWithDeps(blockingLLM{}, tools.ToolExecutor(chatExecutor{}), denyConfirm)
	eng.RehydrateHistory(nil)
	messages := newSyncedMessages()
	sess := store.Session{ID: "sess-1", TopOrganizationID: 1, OrganizationID: 2, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	h := newChatTestHandlersWith(t, eng, &mockSessions{byID: map[string]store.Session{sess.ID: sess}})
	h.messages = messages
	traces := &syncedTraceWriter{}
	h.traceWriter = traces
	base := BaseRequest{Action: "SendCSAgentChat", RequestUUID: "request-lifetime"}
	base.Owner = store.Owner{TopOrganizationID: 1, OrganizationID: 2}
	prep, apiErr := h.prepareChat(context.Background(), base, sess.ID, "hi", "")
	require.Nil(t, apiErr)
	defer prep.release()
	streamCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	sink := &recordingSink{}

	h.chatStream(streamCtx, sink, base, prep)

	patch := messages.waitPatch(t, time.Second)
	require.Equal(t, "aborted", patch.Status)
	require.Nil(t, patch.ErrorCode, "a transport backstop is not a model error")
	require.Equal(t, abortedAssistantMessage, patch.Content)
	require.False(t, sink.has("error"), "there is no client left to receive an error frame")
	require.Equal(t, observability.TerminatedByTimeout, traces.last(t).Outcome.TerminatedBy)
}

func TestAbortedTurnContent(t *testing.T) {
	require.Equal(t, abortedAssistantMessage, abortedTurnContent("", ""))
	require.Equal(t, abortedAssistantMessage+"\n\n本轮已完成的操作：\n✅ 已为实例 uhost-1 执行开机。",
		abortedTurnContent(" ✅ 已为实例 uhost-1 执行开机。 ", ""))
	require.Equal(t, abortedAssistantMessage+"\n\n本轮对实例 uhost-1 的实例内排查没有正常结束。",
		abortedTurnContent("", "本轮对实例 uhost-1 的实例内排查没有正常结束。"))
	both := abortedTurnContent("✅ 已创建实例 uhost-9。", "排查摘要")
	require.Less(t, strings.Index(both, "uhost-9"), strings.Index(both, "排查摘要"), "committed writes come first")
}
