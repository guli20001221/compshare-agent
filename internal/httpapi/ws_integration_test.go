package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/tools"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wsTestHandlers builds Handlers wired with the given LLM and one seeded session,
// plus a started httptest server mounting GET / → HandleWS. It returns the
// *Handlers too so confirm-routing tests can reach the broker directly.
func wsTestHandlers(t *testing.T, client engine.LLMClient, confirm engine.ConfirmFunc) (*httptest.Server, *recordingMessages, *Handlers) {
	t.Helper()
	eng := engine.NewWithDeps(client, tools.ToolExecutor(chatExecutor{}), confirm)
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
	router := gin.New()
	router.GET("/", h.HandleWS)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv, messages, h
}

func wsURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http") + "/?Action=CreateCSAgentWS"
}

// dialWS opens a WS connection carrying gateway identity headers.
func dialWS(t *testing.T, srv *httptest.Server, headers http.Header) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, wsURL(srv.URL), &websocket.DialOptions{HTTPHeader: headers})
	require.NoError(t, err)
	return conn
}

// gatewayHeaders mirrors the identity headers the WS gateway injects on the
// upgrade request (X-Company-Id → top org, X-Organization-Id → org).
func gatewayHeaders() http.Header {
	h := http.Header{}
	h.Set("X-Company-Id", "1")
	h.Set("X-Organization-Id", "2")
	h.Set("X-Request-Id", "req-ws-1")
	return h
}

// gatewayOwner is the store.Owner that gatewayHeaders resolves to.
var gatewayOwner = store.Owner{TopOrganizationID: 1, OrganizationID: 2}

// readFrames reads JSON frames until a terminal event (done/error) or the
// deadline, returning the ordered event names and the last full frame.
func readFrames(t *testing.T, conn *websocket.Conn) ([]string, map[string]any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var events []string
	var last map[string]any
	for {
		_, data, err := conn.Read(ctx)
		require.NoError(t, err)
		var f map[string]any
		require.NoError(t, json.Unmarshal(data, &f))
		ev, _ := f["event"].(string)
		events = append(events, ev)
		last = f
		if ev == "done" || ev == "error" {
			return events, last
		}
	}
}

// TestWS_Handshake_RejectsMissingIdentity proves identity is taken from the
// upgrade headers (not the frame): a connection without X-Company-Id must be
// refused before the socket opens.
func TestWS_Handshake_RejectsMissingIdentity(t *testing.T) {
	srv, _, _ := wsTestHandlers(t, chatLLM{}, denyConfirm)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := http.Header{}
	h.Set("X-Organization-Id", "2") // missing X-Company-Id (top org)
	_, resp, err := websocket.Dial(ctx, wsURL(srv.URL), &websocket.DialOptions{HTTPHeader: h})
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestWS_Handshake_RejectsWrongAction proves the handshake contract is hard:
// GET / only serves CreateCSAgentWS. A valid-identity upgrade declaring a
// different Action is rejected with 400 before the socket opens.
func TestWS_Handshake_RejectsWrongAction(t *testing.T) {
	srv, _, _ := wsTestHandlers(t, chatLLM{}, denyConfirm)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Valid identity headers, but Action=SomethingElse instead of CreateCSAgentWS.
	wrongURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/?Action=SomethingElse"
	_, resp, err := websocket.Dial(ctx, wrongURL, &websocket.DialOptions{HTTPHeader: gatewayHeaders()})
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestWS_Chat_StreamsMetaTokenDone proves a chat turn streams over WS with the
// same frames the SSE path emitted (meta → token → done) and persists the
// assistant reply.
func TestWS_Chat_StreamsMetaTokenDone(t *testing.T) {
	srv, messages, _ := wsTestHandlers(t, chatLLM{}, denyConfirm)
	conn := dialWS(t, srv, gatewayHeaders())
	defer conn.Close(websocket.StatusNormalClosure, "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, conn.Write(ctx, websocket.MessageText,
		[]byte(`{"Action":"SendCSAgentChat","SessionId":"sess-1","Message":"hi"}`)))

	events, _ := readFrames(t, conn)
	assert.Contains(t, events, "meta")
	assert.Contains(t, events, "token")
	assert.Contains(t, events, "done")
	require.Len(t, messages.appended, 2)
	assert.Equal(t, "你好", messages.patch.Content)
	assert.Equal(t, "ok", messages.patch.Status)
}

// TestWS_Confirm_FrameResolvesBrokerWaiter proves the behavior that justifies
// the refactor: a ConfirmCSAgentAction frame sent on the SAME socket resolves a
// pending confirmation for the connection's owner — no second HTTP request.
// This exercises the real read loop, the real ConfirmBroker, and the owner
// check. (The engine deciding to *request* confirmation is covered by the
// engine/saga tests; here we register the pending confirm as chatStream's
// ConfirmFunc would, then prove the socket frame unblocks it.)
func TestWS_Confirm_FrameResolvesBrokerWaiter(t *testing.T) {
	srv, _, h := wsTestHandlers(t, chatLLM{}, denyConfirm)
	conn := dialWS(t, srv, gatewayHeaders())
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Register a pending confirmation for the connection's owner and block a
	// goroutine on it, exactly as chatStream's ConfirmFunc does mid-turn.
	confirmID, ch := h.confirmBroker.Register("sess-1", gatewayOwner)
	result := make(chan bool, 1)
	go func() {
		result <- WaitForConfirmation(context.Background(), ch, 5*time.Second).Confirmed
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, conn.Write(ctx, websocket.MessageText,
		[]byte(`{"Action":"ConfirmCSAgentAction","SessionId":"sess-1","ConfirmationId":"`+confirmID+`","Confirmed":true}`)))

	select {
	case got := <-result:
		assert.True(t, got, "confirm sent over the WS socket must unblock the waiter with true")
	case <-time.After(3 * time.Second):
		t.Fatal("confirmation frame on the socket did not resolve the broker waiter")
	}
}

// TestWS_Confirm_WrongOwnerRejected proves the owner check survives the WS path:
// a confirm frame whose connection owner does not match the pending confirm's
// owner must not resolve it (no cross-tenant confirmation hijack).
func TestWS_Confirm_WrongOwnerRejected(t *testing.T) {
	srv, _, h := wsTestHandlers(t, chatLLM{}, denyConfirm)
	conn := dialWS(t, srv, gatewayHeaders()) // owner {1,2}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Pending confirm belongs to a DIFFERENT owner than the connection.
	otherOwner := store.Owner{TopOrganizationID: 99, OrganizationID: 98}
	confirmID, ch := h.confirmBroker.Register("sess-1", otherOwner)
	result := make(chan bool, 1)
	go func() {
		result <- WaitForConfirmation(context.Background(), ch, 1*time.Second).Confirmed
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, conn.Write(ctx, websocket.MessageText,
		[]byte(`{"Action":"ConfirmCSAgentAction","SessionId":"sess-1","ConfirmationId":"`+confirmID+`","Confirmed":true}`)))

	// The mismatched owner means Resolve fails; the waiter times out (false)
	// rather than being hijacked into true.
	select {
	case got := <-result:
		assert.False(t, got, "a confirm from the wrong owner must not resolve another tenant's confirmation")
	case <-time.After(3 * time.Second):
		t.Fatal("waiter did not return")
	}
}
