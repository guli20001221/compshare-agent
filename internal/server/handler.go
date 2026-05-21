package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/prompt"

	"github.com/google/uuid"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

const (
	// wsReadMessageLimit caps the size of one Client→Server frame at 1 MiB
	// — well above any realistic user message but defeats unbounded-frame
	// DoS attempts.
	wsReadMessageLimit = 1 << 20

	// sendChanBuffer trades a tiny amount of memory per session for the
	// reader/writer goroutines to avoid lock-stepping on a single frame.
	sendChanBuffer = 32

	// writeDeadline applies to each individual writer-loop frame write.
	// If a single frame can't be drained in this window the client is
	// slow / gone and we close the connection.
	writeDeadline = 5 * time.Second

	// chatTurnTimeout caps a single user_message → answer_final round-
	// trip including all tool calls. Tuned for the heaviest workflow
	// (multi-step diagnosis with monitor + diagnosis + analysis tools).
	chatTurnTimeout = 90 * time.Second
)

// handleWS upgrades the request, authenticates the tenant, creates a
// per-connection engine session, and runs the reader/writer goroutines.
// It blocks until the connection closes (client disconnect, error, or
// graceful shutdown).
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if s.shuttingDown.Load() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}

	tenant, err := s.parseTenant(r)
	if err != nil {
		// 401 BEFORE upgrade — keeps the client's library from getting
		// confused by a fully-established WS that immediately closes.
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: s.allowedOrigins,
		// CompressionMode left at default (CompressionDisabled) so we
		// don't pay deflate CPU per frame.
	})
	if err != nil {
		// Accept already wrote a response; log + return.
		log.Printf("ws accept failed for tenant=%d/%d: %v", tenant.TopOrgID, tenant.OrgID, err)
		return
	}
	conn.SetReadLimit(wsReadMessageLimit)
	defer conn.CloseNow()

	connectionID := uuid.NewString()
	sendChan := make(chan ServerMessage, sendChanBuffer)
	bridge := newConfirmBridge()

	var currentReqUUID atomicString
	sess := engine.NewSession(s.deps, engine.SessionOptions{
		Subject:              tenant.Subject(),
		ConfirmFn:            bridge.ConfirmFn(sendChan, currentReqUUID.get),
		MutatingToolsEnabled: false, // first version: read-only path only
	})

	// Init: collect opener suggestions for the ready frame.
	initCtx, initCancel := context.WithTimeout(r.Context(), 15*time.Second)
	suggestions, initErr := sess.Init(initCtx)
	initCancel()
	if initErr != nil {
		log.Printf("session init warning tenant=%d/%d: %v", tenant.TopOrgID, tenant.OrgID, initErr)
	}

	readyMsg := ServerMessage{
		Type:        ServerMsgReady,
		SessionID:   connectionID,
		Suggestions: convertSuggestions(suggestions),
	}
	sendChan <- readyMsg

	// Writer goroutine: drains sendChan to the WS until ctx cancels OR
	// sendChan closes (which the reader does on exit).
	writerDone := make(chan struct{})
	go s.writerLoop(r.Context(), conn, sendChan, writerDone)

	// Reader loop: blocks on conn.Read. Exits on close, error, or
	// shutdown. On exit it closes sendChan so the writer drains.
	s.readerLoop(r.Context(), conn, sess, tenant, connectionID, sendChan, bridge, &currentReqUUID)

	close(sendChan)
	<-writerDone
}

// readerLoop reads frames until the connection closes. Each user_message
// becomes one Chat round-trip; confirm_response frames are routed to the
// bridge; ping replies pong; everything else is an error frame.
func (s *Server) readerLoop(
	ctx context.Context,
	conn *websocket.Conn,
	sess *engine.Engine,
	tenant TenantCtx,
	connectionID string,
	sendChan chan<- ServerMessage,
	bridge *confirmBridge,
	currentReqUUID *atomicString,
) {
	turnIndex := 0
	for {
		var msg ClientMessage
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			if !isExpectedClose(err) {
				log.Printf("ws read error tenant=%d/%d conn=%s: %v",
					tenant.TopOrgID, tenant.OrgID, connectionID, err)
			}
			return
		}
		switch msg.Type {
		case ClientMsgPing:
			tryEnqueue(sendChan, ServerMessage{Type: ServerMsgPong})
		case ClientMsgConfirmResponse:
			if !bridge.OnClientResponse(msg.RequestUUID, msg.Confirmed) {
				log.Printf("orphan confirm_response tenant=%d/%d uuid=%q",
					tenant.TopOrgID, tenant.OrgID, msg.RequestUUID)
			}
		case ClientMsgUserMessage:
			turnIndex++
			currentReqUUID.set(msg.RequestUUID)
			s.runChatTurn(ctx, sess, tenant, connectionID, turnIndex, msg, sendChan)
		default:
			tryEnqueue(sendChan, ServerMessage{
				Type:        ServerMsgError,
				RequestUUID: msg.RequestUUID,
				Code:        ErrCodeProtocolViolation,
				Message:     fmt.Sprintf("unknown client message type %q", msg.Type),
			})
		}
	}
}

// runChatTurn drives one user_message round-trip: pipe tool events into
// sendChan, wait for Engine.Chat, push answer_final + done. Trace writes
// happen via observers attached for the duration of the call so the
// recorder sees this turn's events only.
func (s *Server) runChatTurn(
	ctx context.Context,
	sess *engine.Engine,
	tenant TenantCtx,
	connectionID string,
	turnIndex int,
	msg ClientMessage,
	sendChan chan<- ServerMessage,
) {
	turnCtx, cancel := context.WithTimeout(ctx, chatTurnTimeout)
	defer cancel()

	// Attach a per-turn trace recorder if a sink is configured. The
	// recorder forwards observer callbacks into a TraceRecord and writes
	// it to s.traceSink (file or MySQL) at turn end.
	recorder := newServerTraceRecorder(s.traceSink, tenant, connectionID, turnIndex, msg, s.model)
	if recorder != nil {
		recorder.attach(sess)
	}

	onStep := func(ev engine.StepEvent) {
		switch ev.Type {
		case engine.StepToolCall:
			tryEnqueue(sendChan, ServerMessage{
				Type:        ServerMsgToolCall,
				RequestUUID: msg.RequestUUID,
				Action:      ev.Action,
			})
		case engine.StepToolResult:
			ok := true
			tryEnqueue(sendChan, ServerMessage{
				Type:        ServerMsgToolResult,
				RequestUUID: msg.RequestUUID,
				Action:      ev.Action,
				OK:          &ok,
			})
		case engine.StepBlocked:
			ok := false
			tryEnqueue(sendChan, ServerMessage{
				Type:        ServerMsgToolResult,
				RequestUUID: msg.RequestUUID,
				Action:      ev.Action,
				OK:          &ok,
				Message:     ev.Message,
			})
		case engine.StepError:
			tryEnqueue(sendChan, ServerMessage{
				Type:        ServerMsgError,
				RequestUUID: msg.RequestUUID,
				Code:        ErrCodeInternal,
				Message:     ev.Message,
			})
		}
		if recorder != nil {
			recorder.OnStep(ev)
		}
	}

	reply, err := sess.Chat(turnCtx, msg.Text, onStep)

	if recorder != nil {
		recorder.finish(err)
	}

	if err != nil {
		code := ErrCodeInternal
		if errors.Is(err, governance.ErrRateLimited) {
			code = ErrCodeRateLimited
		}
		tryEnqueue(sendChan, ServerMessage{
			Type:        ServerMsgError,
			RequestUUID: msg.RequestUUID,
			Code:        code,
			Message:     err.Error(),
		})
	} else {
		tryEnqueue(sendChan, ServerMessage{
			Type:        ServerMsgAnswerFinal,
			RequestUUID: msg.RequestUUID,
			Text:        reply,
		})
	}
	tryEnqueue(sendChan, ServerMessage{
		Type:        ServerMsgDone,
		RequestUUID: msg.RequestUUID,
	})
}

// writerLoop drains sendChan to the WebSocket with a per-frame deadline.
// On context cancel, sendChan close, or write error it exits, closing
// the WS so the reader sees an error and exits too.
func (s *Server) writerLoop(ctx context.Context, conn *websocket.Conn, sendChan <-chan ServerMessage, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			_ = conn.Close(websocket.StatusGoingAway, "server context cancelled")
			return
		case msg, ok := <-sendChan:
			if !ok {
				_ = conn.Close(websocket.StatusNormalClosure, "")
				return
			}
			writeCtx, cancel := context.WithTimeout(ctx, writeDeadline)
			err := wsjson.Write(writeCtx, conn, msg)
			cancel()
			if err != nil {
				_ = conn.Close(websocket.StatusInternalError, "write failed")
				return
			}
		}
	}
}

// tryEnqueue pushes to sendChan with a short timeout. The buffered
// channel handles bursts; the timeout catches a stalled writer (client
// stopped reading) so the reader doesn't block forever and miss further
// frames.
func tryEnqueue(sendChan chan<- ServerMessage, msg ServerMessage) {
	select {
	case sendChan <- msg:
	case <-time.After(writeDeadline):
		log.Printf("sendChan saturated, dropping msg type=%s req=%s", msg.Type, msg.RequestUUID)
	}
}

// isExpectedClose returns true for the close codes we'd expect on a
// normal disconnect (so they don't pollute the error log).
func isExpectedClose(err error) bool {
	status := websocket.CloseStatus(err)
	switch status {
	case websocket.StatusNormalClosure,
		websocket.StatusGoingAway,
		websocket.StatusNoStatusRcvd:
		return true
	}
	return false
}

// convertSuggestions adapts engine.Init's []prompt.Suggestion slice to
// the wire Suggestion type. Stays a small projection rather than aliasing
// so the wire form can diverge from internal prompt schema later without
// rippling.
func convertSuggestions(in []prompt.Suggestion) []Suggestion {
	out := make([]Suggestion, 0, len(in))
	for _, s := range in {
		out = append(out, Suggestion{Text: s.Text})
	}
	return out
}

// atomicString is a tiny sync.Mutex-wrapped string holder for tracking
// the current turn's RequestUUID so the bridge's ConfirmFn closure can
// see it without retaining a reference to the user message.
type atomicString struct {
	mu sync.Mutex
	v  string
}

func (a *atomicString) get() string { a.mu.Lock(); defer a.mu.Unlock(); return a.v }
func (a *atomicString) set(v string) { a.mu.Lock(); a.v = v; a.mu.Unlock() }
