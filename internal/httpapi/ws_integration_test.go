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

func readFrameList(t *testing.T, conn *websocket.Conn) []map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var frames []map[string]any
	for {
		_, data, err := conn.Read(ctx)
		require.NoError(t, err)
		var f map[string]any
		require.NoError(t, json.Unmarshal(data, &f))
		frames = append(frames, f)
		if f["event"] == "done" || f["event"] == "error" {
			return frames
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

func TestWS_Chat_CreatesReplacementForMissingSession(t *testing.T) {
	srv, messages, _ := wsTestHandlers(t, chatLLM{}, denyConfirm)
	conn := dialWS(t, srv, gatewayHeaders())
	defer conn.Close(websocket.StatusNormalClosure, "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, conn.Write(ctx, websocket.MessageText,
		[]byte(`{"Action":"SendCSAgentChat","SessionId":"stale-session","Message":"hi"}`)))

	frames := readFrameList(t, conn)
	var meta map[string]any
	for _, f := range frames {
		if f["event"] == "meta" {
			meta = f
			break
		}
	}
	require.NotNil(t, meta, "meta frame must be emitted")
	assert.Equal(t, "sess-new", meta["SessionId"])
	require.Len(t, messages.appended, 2)
	assert.Equal(t, "sess-new", messages.appended[0].SessionID)
	assert.Equal(t, "sess-new", messages.appended[1].SessionID)
	assert.Equal(t, "done", frames[len(frames)-1]["event"])
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

// TestWS_Confirm_SessionDriftResolves reproduces the production bug and proves
// the fix: stale-session recovery mints a new session id mid-turn, so the
// ConfirmCSAgentAction frame carries a DIFFERENT SessionId ("sess-recovered")
// than the one the confirmation was registered under ("sess-1"), while the
// connection owner is unchanged. Before the fix the broker's session-equality
// check false-rejected this with ErrConfirmationOwner ("[Forbidden] ...
// session/owner") and aborted the deploy; now it must resolve, because the
// confirmation is bound to its ConfirmationId + owner, not the session label.
func TestWS_Confirm_SessionDriftResolves(t *testing.T) {
	srv, _, h := wsTestHandlers(t, chatLLM{}, denyConfirm)
	conn := dialWS(t, srv, gatewayHeaders()) // owner {1,2}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Confirmation registered under the pre-recovery session id, same owner.
	confirmID, ch := h.confirmBroker.Register("sess-1", gatewayOwner)
	result := make(chan bool, 1)
	go func() {
		result <- WaitForConfirmation(context.Background(), ch, 5*time.Second).Confirmed
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Confirm frame carries the post-recovery session id — the drift case.
	require.NoError(t, conn.Write(ctx, websocket.MessageText,
		[]byte(`{"Action":"ConfirmCSAgentAction","SessionId":"sess-recovered","ConfirmationId":"`+confirmID+`","Confirmed":true}`)))

	select {
	case got := <-result:
		assert.True(t, got, "a same-owner confirm under a drifted session label must resolve (not Forbidden)")
	case <-time.After(3 * time.Second):
		t.Fatal("session-drift confirmation frame did not resolve the broker waiter")
	}
}

// A resolved confirmation has to say so on the socket.
//
// Until 2026-08-12 only FAILURES wrote a frame here, which left a client no way
// to distinguish "the server accepted this" from "the bytes left my machine".
// The console resolved that ambiguity the only way it could and marked cards
// 已处理 on ws.send(), so an expired card rendered as handled while a [NotFound]
// arrived beside it saying the opposite. The POST transport has always returned
// this acknowledgement (confirmResponse); this is the same fact on the socket
// the console actually uses.
func TestWS_Confirm_SuccessIsAcknowledgedWithItsConfirmationID(t *testing.T) {
	srv, _, h := wsTestHandlers(t, chatLLM{}, denyConfirm)
	conn := dialWS(t, srv, gatewayHeaders())
	defer conn.Close(websocket.StatusNormalClosure, "")

	confirmID, ch := h.confirmBroker.Register("sess-1", gatewayOwner)
	go func() { WaitForConfirmation(context.Background(), ch, 5*time.Second) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, conn.Write(ctx, websocket.MessageText,
		[]byte(`{"Action":"ConfirmCSAgentAction","SessionId":"sess-1","ConfirmationId":"`+confirmID+`","Confirmed":true}`)))

	frame := readOneFrame(t, ctx, conn)
	assert.Equal(t, "confirmation_ack", frame["event"])
	assert.Equal(t, confirmID, frame["ConfirmationId"],
		"the ack must name the card, or a client with two cards open cannot apply it")
	assert.Equal(t, true, frame["Accepted"])
}

// A decline is still a successful resolution: the server accepted the answer.
// Acknowledging only approvals would leave the console unable to settle the card
// a user deliberately refused.
func TestWS_Confirm_DeclineIsAlsoAcknowledged(t *testing.T) {
	srv, _, h := wsTestHandlers(t, chatLLM{}, denyConfirm)
	conn := dialWS(t, srv, gatewayHeaders())
	defer conn.Close(websocket.StatusNormalClosure, "")

	confirmID, ch := h.confirmBroker.Register("sess-1", gatewayOwner)
	go func() { WaitForConfirmation(context.Background(), ch, 5*time.Second) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, conn.Write(ctx, websocket.MessageText,
		[]byte(`{"Action":"ConfirmCSAgentAction","SessionId":"sess-1","ConfirmationId":"`+confirmID+`","Confirmed":false}`)))

	frame := readOneFrame(t, ctx, conn)
	assert.Equal(t, "confirmation_ack", frame["event"])
	assert.Equal(t, confirmID, frame["ConfirmationId"])
	assert.Equal(t, false, frame["Accepted"])
}

// An expired card's rejection must name the card, must not arrive as an "error"
// frame, and must not put developer English on a customer's screen.
//
// All three come from one production turn (2026-08-17): the user clicked 确认 on an
// SSH card that had already timed out, the server answered "error"/NotFound with
// the sentinel's own text, and the console — which treats ANY "error" frame as the
// end of the turn — closed the socket and killed a 30-minute in-instance diagnosis,
// leaving "[NotFound] confirmation not found or already resolved" as the answer.
// Naming the card (#548) was necessary and not sufficient: a client that does not
// know about the id cannot ignore an "error" frame, it can only fail the turn.
func TestWS_Confirm_ExpiredCardRejectionIsScopedToTheCardAndReadable(t *testing.T) {
	srv, _, _ := wsTestHandlers(t, chatLLM{}, denyConfirm)
	conn := dialWS(t, srv, gatewayHeaders())
	defer conn.Close(websocket.StatusNormalClosure, "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, conn.Write(ctx, websocket.MessageText,
		[]byte(`{"Action":"ConfirmCSAgentAction","SessionId":"sess-1","ConfirmationId":"already-gone","Confirmed":true}`)))

	frame := readOneFrame(t, ctx, conn)
	assert.Equal(t, confirmationErrorEventName, frame["event"])
	assert.NotEqual(t, "error", frame["event"],
		"a card-scoped rejection must never arrive on the frame clients end the turn on")
	assert.Equal(t, "NotFound", frame["Code"])
	assert.Equal(t, "already-gone", frame["ConfirmationId"])

	msg, _ := frame["Message"].(string)
	assert.NotEmpty(t, msg)
	assert.NotContains(t, msg, ErrConfirmationNotFound.Error(),
		"the sentinel's English text is for logs, not for the person who clicked")
	assert.Contains(t, msg, "没有生效", "the reader has to be told their click did nothing")
	// And must NOT be told more than the server knows. ClaimResolution removes the
	// entry on the first successful claim, so this same sentinel covers "the card
	// timed out and nothing happened" AND "an earlier click was accepted and may
	// already be running inside the box". Announcing that nothing ran is a guess,
	// and the guess that invites the user to submit the same thing twice.
	for _, overclaim := range []string{"没有执行任何操作", "没有执行任何命令", "未执行"} {
		assert.NotContains(t, msg, overclaim,
			"NotFound is not proof that nothing ran")
	}
	assert.Contains(t, msg, "请勿重复提交",
		"if we cannot say whether it ran, we have to say not to click it again")
}
