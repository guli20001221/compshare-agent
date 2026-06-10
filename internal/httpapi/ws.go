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
	"github.com/gin-gonic/gin"
)

// maxWSConnLifetime caps a single chat WebSocket connection. The gateway opens
// one connection per chat turn (see frame/src/Frame/AIAssistant/service.js: a
// new WebSocket per chatStream call, closed on done/error/abort), so this is a
// generous turn-level ceiling, not a session lifetime. It backstops a wedged
// connection that the gateway never closes.
const maxWSConnLifetime = 10 * time.Minute

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
	defer conn.CloseNow()

	ctx, cancel := context.WithTimeout(c.Request.Context(), maxWSConnLifetime)
	defer cancel()

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
						break
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
				_ = writer.WriteEvent("error", streamErrorEvent{Code: "InvalidParam", Message: ovErr.Error()})
				continue
			}
			if rErr := h.confirmBroker.Resolve(confirmationID, sessionID, frameBase.Owner, ConfirmDecision{Confirmed: confirmed, Overrides: overrides}); rErr != nil {
				code := "NotFound"
				switch {
				case errors.Is(rErr, ErrConfirmationOwner):
					code = "Forbidden"
				case !errors.Is(rErr, ErrConfirmationNotFound):
					// Override validation failure: pending is kept (the client
					// may fix and resend within the timeout window).
					code = "InvalidParam"
				}
				_ = writer.WriteEvent("error", streamErrorEvent{Code: code, Message: rErr.Error()})
			}

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
