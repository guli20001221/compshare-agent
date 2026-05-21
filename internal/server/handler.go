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
	"github.com/compshare-agent/internal/observability"
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
	// PR11: register this conn so graceful shutdown can actively close it
	// with StatusGoingAway. The cleanup closure runs on every exit path
	// (return, panic) because of the defer.
	defer s.trackConn(connectionID, conn)()
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
	// sendChan closes (which we do on exit, after chatLoop drains).
	writerDone := make(chan struct{})
	go s.writerLoop(r.Context(), conn, sendChan, writerDone)

	// PR10: split reader and chat into separate goroutines so a Chat in
	// progress (especially one blocked in confirmFn waiting for the
	// client's confirm_response frame) does NOT prevent the reader from
	// processing that very frame. Pre-PR10 this caused a 30s deadlock on
	// every confirm round-trip — the reader was stuck inside runChatTurn
	// waiting for Chat to return while Chat was stuck inside confirmFn
	// waiting for the reader to deliver confirm_response.
	turnQueue := make(chan ClientMessage, 1)
	chatDone := make(chan struct{})
	go s.chatLoop(r.Context(), sess, tenant, connectionID, turnQueue, sendChan, &currentReqUUID, chatDone)

	// Reader loop: blocks on conn.Read; dispatches user_message frames to
	// turnQueue (so chatLoop runs them) and confirm_response frames to the
	// bridge (so any in-flight Chat unblocks). Exits on close / error.
	s.readerLoop(r.Context(), conn, tenant, connectionID, sendChan, bridge, turnQueue)

	// Reader exited → no more frames will arrive. Tell chatLoop to drain
	// any queued message (buffer is 1) and exit. We must wait for chatDone
	// BEFORE closing sendChan: chatLoop writes to sendChan from runChatTurn
	// (tool events, answer_final, done) and from the bridge's confirmFn
	// path; closing sendChan while it's still writing would panic.
	close(turnQueue)
	<-chatDone

	close(sendChan)
	<-writerDone
}

// readerLoop reads frames until the connection closes. Each user_message
// is dispatched to turnQueue (so chatLoop processes it serially);
// confirm_response frames are routed directly to the bridge so any
// in-flight Chat unblocks immediately; ping replies pong; everything
// else is an error frame. The reader NEVER calls runChatTurn directly —
// that's chatLoop's job — which is what unblocks the confirm round-trip.
func (s *Server) readerLoop(
	ctx context.Context,
	conn *websocket.Conn,
	tenant TenantCtx,
	connectionID string,
	sendChan chan<- ServerMessage,
	bridge *confirmBridge,
	turnQueue chan<- ClientMessage,
) {
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
			// Route directly to bridge — NOT via turnQueue. The bridge's
			// OnClientResponse is non-blocking (select-default) and exists
			// precisely so this frame can be delivered while chatLoop is
			// blocked inside engine.Chat waiting for it.
			if !bridge.OnClientResponse(msg.RequestUUID, msg.Confirmed) {
				log.Printf("orphan confirm_response tenant=%d/%d uuid=%q",
					tenant.TopOrgID, tenant.OrgID, msg.RequestUUID)
			}
		case ClientMsgUserMessage:
			// Try to enqueue. turnQueue is buffer=1: chatLoop is either
			// idle (instant enqueue) OR running the previous turn (queue
			// holds the next one). If a second user_message arrives while
			// the buffer is also full, the client violated the "one turn
			// at a time" contract — surface as protocol_violation rather
			// than silently dropping or unbounded-buffering.
			select {
			case turnQueue <- msg:
			case <-ctx.Done():
				return
			default:
				tryEnqueue(sendChan, ServerMessage{
					Type:        ServerMsgError,
					RequestUUID: msg.RequestUUID,
					Code:        ErrCodeProtocolViolation,
					Message:     "previous turn still in progress; wait for done before sending next user_message",
				})
			}
			// Note: currentReqUUID is set by chatLoop before invoking Chat —
			// not here — so the value matches the turn actually being
			// processed, not one buffered in turnQueue.
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

// chatLoop drains turnQueue and processes each user_message serially via
// runChatTurn. It owns the turnIndex counter (so per-connection ordering
// is preserved) AND sets currentReqUUID just before invoking Chat so the
// bridge's confirmFn closure always sees the correct active turn. Exits
// when turnQueue is closed (reader returned).
//
// PR10 lifecycle: handleWS closes turnQueue after readerLoop returns; a
// possibly mid-flight Chat continues to completion before chatLoop exits,
// up to chatTurnTimeout. confirmFn's own 30s timeout caps the worst case
// when reader has died with a confirm in flight.
func (s *Server) chatLoop(
	ctx context.Context,
	sess *engine.Engine,
	tenant TenantCtx,
	connectionID string,
	turnQueue <-chan ClientMessage,
	sendChan chan<- ServerMessage,
	currentReqUUID *atomicString,
	done chan<- struct{},
) {
	defer close(done)
	turnIndex := 0
	for msg := range turnQueue {
		turnIndex++
		// Set BEFORE invoking Chat — confirmFn's closure reads this to
		// stamp confirm_required frames with the correct RequestUUID.
		// Setting in chatLoop (rather than the reader) ensures the value
		// reflects the turn actively being processed, not the next one
		// already buffered.
		currentReqUUID.set(msg.RequestUUID)
		s.runChatTurn(ctx, sess, tenant, connectionID, turnIndex, msg, sendChan)
	}
}

// runChatTurn drives one user_message round-trip: pipe tool events into
// sendChan, wait for Engine.Chat, push answer_final + done. Trace writes
// happen via observers attached for the duration of the call so the
// recorder sees this turn's events only.
//
// Per-tenant turn quota (governance.ClassUserTurn) is checked here, ONCE
// per user_message frame. Confirm responses and pings do not pass through
// this function and therefore do not consume the daily turn budget — a
// single mutating-tool confirmation may span multiple frames, and counting
// them as turns would let a user exhaust their quota on confirmations
// alone. The Allow call short-circuits when both UserTurnQPS and
// UserTurnDaily are 0 (operator hasn't opted in).
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

	if dec := s.deps.RateLimiter.Allow(governance.Request{
		SubjectKey: tenant.Subject(),
		Class:      governance.ClassUserTurn,
		Action:     "chat_turn",
		Now:        time.Now(),
	}); !dec.Allowed {
		// Record the rejection in agent_messages so dashboards can
		// count "users hitting the daily cap today". agent_traces is
		// skipped — there's no ReAct trace to record. Status=blocked
		// mirrors the in-engine rate-limit return path.
		if s.msgRecorder != nil {
			// LatencyMS=0 is intentional for synthetic quota-blocked rows:
			// no engine.Chat ran, no LLM was called, so there's no real
			// latency to record. Downstream dashboards computing P50/P95
			// of latency MUST filter to status='success' (or status != 'blocked')
			// to avoid the zero-latency blocked rows skewing the percentiles
			// downward. agent_messages.status='blocked' is the safe filter.
			_ = s.msgRecorder.Record(MessageEntry{
				RequestUUID:      msg.RequestUUID,
				TopOrgID:         tenant.TopOrgID,
				OrgID:            tenant.OrgID,
				ConnectionID:     connectionID,
				CreatedAt:        time.Now(),
				UserMessage:      msg.Text,
				AssistantMessage: userTurnQuotaMessage(dec.Reason),
				Status:           "blocked",
				Model:            s.model,
				LatencyMS:        0,
			})
		}
		tryEnqueue(sendChan, ServerMessage{
			Type:        ServerMsgError,
			RequestUUID: msg.RequestUUID,
			Code:        ErrCodeRateLimited,
			Message:     userTurnQuotaMessage(dec.Reason),
		})
		tryEnqueue(sendChan, ServerMessage{
			Type:        ServerMsgDone,
			RequestUUID: msg.RequestUUID,
		})
		return
	}

	// Attach a per-turn trace recorder if a sink is configured. The
	// recorder forwards observer callbacks into a TraceRecord and writes
	// it to s.traceSink (file or MySQL) at turn end.
	recorder := newServerTraceRecorder(s.traceSink, tenant, connectionID, turnIndex, msg, s.model)
	if recorder != nil {
		recorder.attach(sess)
	}

	onStep := func(ev engine.StepEvent) {
		if m, ok := stepEventToServerMessage(ev, msg.RequestUUID); ok {
			tryEnqueue(sendChan, m)
		}
		if recorder != nil {
			recorder.OnStep(ev)
		}
	}

	chatStart := time.Now()
	reply, err := sess.Chat(turnCtx, msg.Text, onStep)
	latencyMS := int(time.Since(chatStart).Milliseconds())

	// Capture the finalized trace record so we can derive the
	// agent_messages status enum without re-walking the observers.
	// When recorder is nil (no trace sink) we get a zero-value record
	// and DeriveStatus still produces the correct chatErr-only signal.
	var finalTrace observability.TraceRecord
	if recorder != nil {
		finalTrace = recorder.finish(err)
	}

	// Persist the chat turn into agent_messages. Status comes from
	// DeriveStatus, which carries the caller's chatErr through to the
	// "error" enum value.
	//
	// CROSS-TABLE DIVERGENCE (documented, intentional):
	// agent_traces.status is computed by observability.statusFromTrace
	// which has no chatErr channel; when chatErr != nil the trace
	// recorder maps it to EngineHardBlock{Hit:true, Category:"chat_error"}
	// so the trace row reports "blocked", not "error". Dashboards that
	// need the "error" distinction MUST query agent_messages; the
	// success / rate-limit / hard-block paths still join cleanly.
	//
	// Ordering within a connection: agent processes turns serially, so
	// CreatedAt is strictly monotonic per connection_id. The runtime
	// `turnIndex` counter still drives TraceRecord.TurnIndex (observability
	// schema invariant), but no longer lives on agent_messages.
	if s.msgRecorder != nil {
		_ = s.msgRecorder.Record(MessageEntry{
			RequestUUID:      msg.RequestUUID,
			TopOrgID:         tenant.TopOrgID,
			OrgID:            tenant.OrgID,
			ConnectionID:     connectionID,
			CreatedAt:        chatStart,
			UserMessage:      msg.Text,
			AssistantMessage: reply,
			Status:           DeriveStatus(err, finalTrace),
			Model:            s.model,
			LatencyMS:        latencyMS,
		})
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

// stepEventToServerMessage projects an engine.StepEvent into the
// ServerMessage that should be emitted on the WS. Pure function — no
// channel, no I/O — so the protocol mapping (specifically the per-tool-
// error → tool_result(ok=false) rule) can be unit-tested without a
// real Engine. Returns ok=false when the event type has no WS-visible
// projection (defensive default; current event set is fully covered).
//
// SECURITY/protocol invariant: per-tool failures (StepError, StepBlocked)
// map to ServerMsgToolResult+OK=false, NOT ServerMsgError. The top-level
// ServerMsgError frame is reserved for whole-turn failures (engine.Chat
// returned an error). See protocol.go ServerMsg* doc block.
func stepEventToServerMessage(ev engine.StepEvent, requestUUID string) (ServerMessage, bool) {
	switch ev.Type {
	case engine.StepToolCall:
		return ServerMessage{
			Type:        ServerMsgToolCall,
			RequestUUID: requestUUID,
			Action:      ev.Action,
		}, true
	case engine.StepToolResult:
		ok := true
		return ServerMessage{
			Type:        ServerMsgToolResult,
			RequestUUID: requestUUID,
			Action:      ev.Action,
			OK:          &ok,
		}, true
	case engine.StepBlocked:
		ok := false
		return ServerMessage{
			Type:        ServerMsgToolResult,
			RequestUUID: requestUUID,
			Action:      ev.Action,
			OK:          &ok,
			Message:     ev.Message,
		}, true
	case engine.StepError:
		ok := false
		return ServerMessage{
			Type:        ServerMsgToolResult,
			RequestUUID: requestUUID,
			Action:      ev.Action,
			OK:          &ok,
			Message:     ev.Message,
		}, true
	}
	return ServerMessage{}, false
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

// userTurnQuotaMessage formats the user-visible reason for a turn-quota
// denial. Daily and QPS denials read differently — a daily-exhausted user
// should be told tomorrow, a QPS-throttled user told to try again shortly.
func userTurnQuotaMessage(reason governance.Reason) string {
	if reason == governance.ReasonDailyExceeded {
		return "今日提问次数已达上限，请明日再试。"
	}
	return "操作过于频繁，请稍后再试。"
}
