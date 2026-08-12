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
	// minWSMachineLifetime is the FLOOR for the MACHINE half of a single chat WebSocket. The
	// gateway opens one connection per chat turn (see frame/src/Frame/AIAssistant/service.js: a
	// new WebSocket per chatStream call, closed on done/error/abort), so this bounds a turn, not
	// a session. It backstops a wedged connection that the gateway never closes.
	//
	// It was a flat ceiling named maxWSConnLifetime until 2026-07-30, chosen before the
	// write-enabled SSH-ops lane existed. A live frontend run showed the contradiction that
	// created: agent.ssh_ops.timeout was 12m, this was 10m, and an in-instance repair died at
	// exactly 10:00.0 — the user got "[NetworkError] 连接已关闭" and nothing else, after the lane
	// had already replaced an application directory on the box. Two independent numbers governing
	// one turn will drift; see wsConnLifetime, which derives the deadline from the lane budget so
	// the socket always outlives the work it carries.
	minWSMachineLifetime = 10 * time.Minute

	// wsMachineSlack is the MACHINE work a turn does outside the harness subprocess:
	// agent.ssh_ops.timeout bounds only the subprocess, while the same turn also retrieves
	// knowledge and writes the verdict after the harness returns.
	//
	// Until 2026-08-12 this was named wsLaneSlack and its comment also charged "waits on the
	// operator's consent cards" to these two minutes. That was the bug below: it made a person
	// reading an authorization card share a budget sized for machine work, and 2 minutes cannot
	// hold even one card at the current timeout. Human time is now wsInteractionAllowance.
	wsMachineSlack = 2 * time.Minute

	// wsMaxConfirmationsPerTurn is the card count the human-time budget is sized from. It is
	// workflow.MaxConfirmationsPerWorkflowTurn — DERIVED from the wizard's own step order plus its
	// edit cap — and not a number chosen here.
	//
	// It was 6 for one commit, taken from the largest run seen in 30 days of agent_traces, with a
	// comment claiming it covered every card a turn may legitimately show. That was false when it
	// was written: guided create has eleven steps and allows three re-asks, so the code has always
	// permitted fourteen. Sizing a bound from what users happened to do, and then describing it as
	// what the system permits, is how a budget silently becomes the thing that fails.
	wsMaxConfirmationsPerTurn = workflow.MaxConfirmationsPerWorkflowTurn

	// wsInteractionAllowance is the socket headroom reserved for HUMAN time, kept deliberately
	// separate from every machine budget above.
	//
	// agent.ssh_ops.timeout states how long the MACHINE may work inside an instance. It says
	// nothing about how long a PERSON may take to read a card that authorizes writing to their
	// box, and deriving one from the other would make a careful reader look like a slow harness.
	// The socket has to cover the sum, so the sum is what it is built from — machine budget first,
	// then this, never one folded into the other.
	//
	// What this is NOT: a guarantee that every legal turn fits. It covers the bounded flow
	// (workflows, above) with certainty. The in-instance ops lane asks once per mutating command
	// and has NO ceiling on how many that is — the harness's own 50-step cap is the only limit, so
	// a repair that stopped for fifty separate approvals could still outlive the socket. That is a
	// deliberate backstop, not an oversight: a turn parked on its fifteenth human approval is a
	// wedged connection by any useful definition, and the connection deadline exists to end those.
	// Stated here rather than left for someone to discover from a closed socket.
	wsInteractionAllowance = wsMaxConfirmationsPerTurn * confirmWaitTimeout

	// maxWSMessageBytes must fit screenshot uploads. OCR accepts up to 10 MiB raw
	// image bytes; base64 plus JSON framing needs extra room on the WebSocket.
	maxWSMessageBytes int64 = 20 * 1024 * 1024
)

// wsConnLifetime is the deadline for one chat socket: the machine budget for the turn, PLUS the
// human time its authorization cards may take.
//
// Deriving the machine half is the point. agent.ssh_ops.timeout is an operator's statement of how
// long a single turn may legitimately spend inside an instance, so a transport deadline shorter
// than that does not protect anything — it just kills the longest, most consequential turns, which
// under allow_writes are the ones that have already changed the box. Deliberately NOT clamped to a
// second ceiling: a cap that silently cut a longer configured lane short would be this same bug
// with a bigger number.
//
// Adding the interaction allowance separately is the 2026-08-12 fix. Production traces showed 36
// of 292 confirmation cards (12.3%) ending at exactly the confirm timeout, with real confirmations
// landing 168ms before the wall — a live distribution being clipped, not users walking away.
// Raising that timeout without giving the socket its own room for human time would just move the
// wall onto the transport.
func (h *Handlers) wsConnLifetime() time.Duration {
	return h.wsMachineLifetime() + wsInteractionAllowance
}

// wsMachineLifetime is the machine half: the wedged-connection floor, or the configured
// in-instance lane budget plus non-harness machine slack when that is longer.
func (h *Handlers) wsMachineLifetime() time.Duration {
	if h == nil || h.cfg == nil {
		return minWSMachineLifetime
	}
	// Keyed off the configured budget rather than the enabled flag: the lane can also be switched
	// on by env (COMPSHARE_SSH_OPS) with no YAML `enabled`, and an operator who wrote a timeout has
	// declared the length of a turn either way. When the lane is off the field is unset, so this
	// reads the floor.
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

	writer := wsx.New(ctx, conn)
	if h.turnCoordinator != nil {
		// Durable mode has its own detachable subscription loop. Closing this
		// socket only drops the observer; the coordinator owns execution and
		// continues until it records a terminal result.
		h.handleDurableWS(ctx, cancel, connBase, conn, writer)
		return
	}

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
				_ = writer.WriteEvent("error", confirmationErrorEvent{
					Code: "InvalidParam", Message: ovErr.Error(), ConfirmationID: confirmationID,
				})
				continue
			}
			h.resolveConfirmFrame(writer, confirmationID, sessionID, frameBase.Owner,
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
// The ORDER is the contract. Waking first is a race this goroutine cannot win —
// the decision unblocks the chat goroutine, which may finish the turn, write
// `done` and cancel the connection context before the acknowledgement reaches
// the socket. The client would then be told that an accepted, already-executed
// action had failed. It is a separate function so that order is testable at all:
// driven through the real WebSocket, both orders look identical from outside.
func (h *Handlers) resolveConfirmFrame(w streamWriter, confirmationID, sessionID string, owner store.Owner, decision ConfirmDecision) {
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
		// Name the card. The error frame used to carry only a code and a sentence,
		// so a client could not tell which of several cards it rejected — and an
		// expired card therefore stayed rendered as accepted while the turn went
		// red somewhere else.
		_ = w.WriteEvent("error", confirmationErrorEvent{
			Code: code, Message: err.Error(), ConfirmationID: confirmationID,
		})
		return
	}
	// The acknowledgement the socket never had. A client may now wait for THIS
	// before showing the card as handled, instead of treating its own ws.send()
	// as proof the server agreed.
	_ = w.WriteEvent("confirmation_ack", confirmationAckEvent{
		ConfirmationID: confirmationID, Accepted: decision.Confirmed,
	})
	deliver()
}
