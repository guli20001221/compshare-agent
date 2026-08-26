package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bitly/go-simplejson"
	"github.com/coder/websocket"
	wsx "github.com/compshare-agent/internal/httpapi/ws"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/workflow"
	"github.com/gin-gonic/gin"
)

const (
	// minWSMachineLifetime bounds one chat turn, not a session, and backstops a
	// connection the gateway never closes.
	minWSMachineLifetime = 10 * time.Minute

	// wsMachineSlack covers work around the SSH harness subprocess.
	wsMachineSlack = 2 * time.Minute

	// Derived from the workflow's step count and edit allowance.
	wsMaxConfirmationsPerTurn = workflow.MaxConfirmationsPerWorkflowTurn

	// Human confirmation time is independent of machine execution time. This
	// covers bounded workflows; the SSH lane's task-scope authorization remains
	// subject to the overall connection backstop.
	wsInteractionAllowance = wsMaxConfirmationsPerTurn * confirmWaitTimeout

	// maxWSMessageBytes must fit screenshot uploads. OCR accepts up to 10 MiB raw
	// image bytes; base64 plus JSON framing needs extra room on the WebSocket.
	maxWSMessageBytes int64 = 20 * 1024 * 1024
)

// wsConnLifetime is the sum of the machine budget and independent human
// confirmation allowance. It is not clamped below the configured SSH budget.
func (h *Handlers) wsConnLifetime() time.Duration {
	return h.wsMachineLifetime() + wsInteractionAllowance
}

// wsMachineLifetime is the machine half: the wedged-connection floor, or the configured
// in-instance lane budget plus non-harness machine slack when that is longer.
func (h *Handlers) wsMachineLifetime() time.Duration {
	if h == nil || h.cfg == nil {
		return minWSMachineLifetime
	}
	// A configured timeout declares the maximum legitimate lane duration. When
	// the lane is absent the field is zero and the floor applies.
	if lane := h.cfg.Agent.SSHOps.Timeout; lane > 0 && lane+wsMachineSlack > minWSMachineLifetime {
		return lane + wsMachineSlack
	}
	return minWSMachineLifetime
}

// HandleWS upgrades a gateway-initiated request to a WebSocket and serves the
// streaming chat protocol. Identity comes from the upgrade request's HTTP
// headers (a WS upgrade is a bodyless GET — the gateway cannot inject a JSON
// body), parsed by ParseBaseRequestFromHeaders. The query carries Action; the
// handshake Action is CreateCSAgentWS.
//
// Frame protocol on the open socket (one JSON text message per frame):
//   - inbound  SendCSAgentChat      → run a chat turn, stream meta/step/token/done
//   - inbound  ConfirmCSAgentAction → resolve the in-flight turn's confirmation
//   - outbound {event, ...payload}  (see ws.Writer)
//
// Concurrency: the read loop runs in this goroutine and must keep reading while
// a chat turn blocks on a confirmation, so the confirm frame can arrive. The
// chat turn therefore runs in its own goroutine; its ConfirmFunc blocks on the
// transport-agnostic ConfirmBroker, which the read loop unblocks via Resolve.
func (h *Handlers) HandleWS(c *gin.Context) {
	connBase, err := ParseBaseRequestFromHeaders(c.Request)
	if err != nil {
		// Pre-upgrade: respond with a normal HTTP error so the gateway sees a
		// 4xx rather than a half-open socket.
		h.writeError(c, connBase.Action, connBase.RequestUUID, err)
		return
	}
	// Harden the handshake contract: GET / only serves the chat WebSocket, whose
	// gateway Action is CreateCSAgentWS. Reject any other Action before upgrading
	// so the endpoint cannot be opened under a different/blank Action.
	if connBase.Action != "CreateCSAgentWS" {
		h.writeError(c, connBase.Action, connBase.RequestUUID,
			ErrInvalidParam.WithMessage("unsupported WebSocket Action %q (want CreateCSAgentWS)", connBase.Action))
		return
	}

	conn, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{
		// The gateway is a trusted server-side origin (not a browser). Origin
		// enforcement happens at the gateway; the agent only ever receives
		// gateway-proxied connections.
		InsecureSkipVerify: true,
	})
	if err != nil {
		log.Printf("warning: websocket accept failed (request %s): %v", connBase.RequestUUID, err)
		return
	}
	conn.SetReadLimit(maxWSMessageBytes)
	defer conn.CloseNow()

	ctx, cancel := context.WithTimeout(c.Request.Context(), h.wsConnLifetime())
	defer cancel()
	// net/http treats an upgraded WebSocket as hijacked and Server.Shutdown does
	// not wait for it. Register the connection so process shutdown can cancel the
	// turn and wait for chatStream's persistence defers before exiting.
	wsDone := make(chan struct{})
	wsID, accepted := h.registerWebSocket(cancel, wsDone)
	if !accepted {
		return
	}
	defer h.unregisterWebSocket(wsID)
	defer close(wsDone)

	writer := wsx.New(ctx, conn)
	// One chat turn per socket. The frontend opens a fresh WebSocket per
	// chatStream call and closes it on done/error (service.js), and the gateway
	// mirrors that one-to-one, so a connection serves exactly one SendCSAgentChat
	// (plus its confirmation round-trip). When the turn completes, the chat
	// goroutine cancels the connection context, which unblocks conn.Read below
	// and tears the socket down — there is no second turn to bookkeep, which
	// removes the race a reset-flag approach would have at the turn boundary.
	//
	// `started` is touched only by this read-loop goroutine. The chat turn runs
	// in its own goroutine so the read loop keeps reading and can deliver the
	// ConfirmCSAgentAction frame while the turn blocks in its ConfirmFunc.
	started := false
	chatDone := make(chan struct{}, 1)

	// On exit, cancel the turn context and wait for the chat goroutine to unwind
	// before the deferred CloseNow runs — otherwise it could write to a closed
	// conn. cancel() makes the engine + WaitForConfirmation observe ctx.Done()
	// promptly, so the wait is bounded.
	defer func() {
		cancel()
		if started {
			<-chatDone
		}
	}()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			// Normal closure, client going away, turn-complete cancel, or
			// deadline — all terminal.
			return
		}

		frame, jErr := simplejson.NewJson(data)
		if jErr != nil {
			_ = writer.WriteEvent("error", streamErrorEvent{Code: "InvalidParam", Message: "invalid frame json"})
			continue
		}

		// Identity is connection-scoped (from headers); per-frame business
		// fields (Action / SessionId / Message / ConfirmationId) come from the
		// frame body. Each frame gets its own request_uuid for tracing unless
		// it carries one.
		frameBase := connBase
		frameBase.Action = frame.Get("Action").MustString()
		if rid := frame.Get("request_uuid").MustString(); rid != "" {
			frameBase.RequestUUID = rid
		}

		switch frameBase.Action {
		case "SendCSAgentChat":
			if started {
				// A turn was already started on this socket; one-turn-per-socket
				// means this is unexpected (genuinely concurrent send).
				_ = writer.WriteEvent("error", streamErrorEvent{
					Code: "Conflict", Message: "a chat turn is already in progress on this connection",
				})
				continue
			}
			sessionID := frame.Get("SessionId").MustString()
			message := frame.Get("Message").MustString()
			imageDataURL := frame.Get("Image").MustString()
			if pid := frame.Get("ProjectId").MustString(); pid != "" {
				frameBase.ProjectID = pid
			}

			prep, apiErr := h.prepareChat(ctx, frameBase, sessionID, strings.TrimSpace(message), strings.TrimSpace(imageDataURL))
			if apiErr != nil {
				_ = writer.WriteEvent("error", streamErrorEvent{Code: apiErr.Code, Message: apiErr.Message})
				continue
			}
			// Editable-confirm-form opt-in (create-flow 表单化): per-turn client
			// capability declaration. Absent/unknown values leave it off, so
			// legacy clients keep byte-identical confirmation frames.
			if features, err := frame.Get("Features").StringArray(); err == nil {
				for _, f := range features {
					if f == featureConfirmForm {
						prep.confirmFormOptIn = true
					}
					if f == featureGuidedCreate {
						prep.guidedCreateOptIn = true
					}
					if f == featureKnowledgeOnly {
						prep.knowledgeOnlyOptIn = true
					}
					if f == featureFeishuPublicPlatformReadOnly {
						prep.publicPlatformReadOnlyOptIn = true
					}
					if f == featureFeishuConsoleHandoff {
						prep.feishuConsoleHandoffOptIn = true
					}
				}
			}

			started = true
			go func(base BaseRequest, prep *chatPrep) {
				defer prep.release()
				// Tearing down the socket after the turn enforces one-turn-per-
				// socket: cancel() unblocks the read loop's conn.Read, which then
				// returns. chatStream has already written its done/error frame
				// before returning, so cancel never truncates output.
				defer cancel()
				defer func() { chatDone <- struct{}{} }()
				h.chatStream(ctx, writer, base, prep)
			}(frameBase, prep)

		case "ConfirmCSAgentAction":
			if h.confirmBroker == nil {
				_ = writer.WriteEvent("error", streamErrorEvent{Code: "Internal", Message: "confirmation not supported"})
				continue
			}
			sessionID := frame.Get("SessionId").MustString()
			confirmationID := frame.Get("ConfirmationId").MustString()
			confirmed := frame.Get("Confirmed").MustBool(false)
			// Optional select-only field overrides (editable confirm form).
			// Strictly string-valued: a malformed entry rejects the WHOLE frame
			// (error + keep pending) rather than silently dropping the edit and
			// confirming the unedited card.
			overrides, ovErr := overridesFromFrame(frame)
			if ovErr != nil {
				// Name the card here too. This frame is scoped to one card by
				// construction, and without the id a client can only fail the whole
				// turn — which would let a single malformed form field end a
				// diagnosis, the same shape as the expired-card bug below.
				// The parse detail ("Overrides.X must be a string") names a wire field
				// no customer authored; it goes to the log, not to their screen.
				log.Printf("confirm %s: malformed Overrides rejected (%v)", confirmationID, ovErr)
				_ = writer.WriteEvent(confirmationErrorEventName, confirmationErrorEvent{
					Code: "InvalidParam", Message: confirmationFailureMessage(ovErr), ConfirmationID: confirmationID,
				})
				continue
			}
			h.resolveConfirmFrame(writer, cancel, confirmationID, sessionID, frameBase.Owner,
				ConfirmDecision{Confirmed: confirmed, Overrides: overrides})

		default:
			_ = writer.WriteEvent("error", streamErrorEvent{
				Code: "InvalidParam", Message: "unsupported Action " + frameBase.Action,
			})
		}
	}
}

// overridesFromFrame parses the optional Overrides object. Missing is allowed
// and means "confirm without edits"; present-but-not-object is invalid because
// silently treating it as empty would confirm the unedited card.
func overridesFromFrame(frame *simplejson.Json) (map[string]string, error) {
	raw, ok := frame.CheckGet("Overrides")
	if !ok {
		return nil, nil
	}
	m, err := raw.Map()
	if err != nil {
		return nil, fmt.Errorf("Overrides must be an object")
	}
	return stringMapFromFrame(m)
}

// stringMapFromFrame coerces a JSON-decoded object to map[string]string,
// erroring on any non-string value (a malformed override must reject the
// frame, not silently degrade to a no-edit confirm). nil/empty in → nil out.
func stringMapFromFrame(m map[string]any) (map[string]string, error) {
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("Overrides.%s must be a string", k)
		}
		out[k] = s
	}
	return out, nil
}

// resolveConfirmFrame answers one ConfirmCSAgentAction: it claims the decision,
// writes the outcome frame, and only then wakes the turn.
//
// The order is the contract: the client must receive the acknowledgement before
// the decision can wake and complete the turn.
//
// An acknowledgement that cannot be WRITTEN is fail-closed: the decision is
// dropped and cancelTurn ends the connection, so the waiting turn resolves as
// client_disconnect and executes nothing. Ignoring that write error would give
// the worst outcome of both designs — the user sees "服务端未在预期时间内回应"
// after CONFIRM_ACK_TIMEOUT_MS while the box is being changed behind the dead
// socket. The point of the acknowledgement is that the client and the server
// agree on what was authorized; if it cannot be delivered, there is no agreement
// to act on. (Only a broken/cancelled socket can fail here — see ws.Writer.)
func (h *Handlers) resolveConfirmFrame(w streamWriter, cancelTurn context.CancelFunc, confirmationID, sessionID string, owner store.Owner, decision ConfirmDecision) {
	deliver, err := h.confirmBroker.ClaimResolution(confirmationID, sessionID, owner, decision)
	if err != nil {
		code := "NotFound"
		switch {
		case errors.Is(err, ErrConfirmationOwner):
			code = "Forbidden"
		case !errors.Is(err, ErrConfirmationNotFound):
			// Override validation failure: pending is kept (the client may fix and
			// resend within the timeout window).
			code = "InvalidParam"
		}
		// Scope the failure to the affected card; it must not terminate the turn.
		log.Printf("confirm %s: resolve rejected (%v)", confirmationID, err)
		_ = w.WriteEvent(confirmationErrorEventName, confirmationErrorEvent{
			Code: code, Message: confirmationFailureMessage(err), ConfirmationID: confirmationID,
		})
		return
	}
	// The acknowledgement the socket never had. A client may now wait for THIS
	// before showing the card as handled, instead of treating its own ws.send()
	// as proof the server agreed.
	if err := w.WriteEvent("confirmation_ack", confirmationAckEvent{
		ConfirmationID: confirmationID, Accepted: decision.Confirmed,
	}); err != nil {
		log.Printf("confirm %s: acknowledgement undeliverable (%v) — dropping the decision and ending the turn", confirmationID, err)
		cancelTurn()
		return
	}
	deliver()
}
