package httpapi

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/bitly/go-simplejson"
	"github.com/coder/websocket"
	"github.com/compshare-agent/internal/ocr"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/turncoord"
)

const (
	durableTurnProtocolVersion = 2
	featureTurnReplay          = "turn_replay_v2"
)

// handleDurableWS is the only chat writer when a coordinator is installed.
// The connection is merely a detachable observer: its context scopes replay
// and writes, never the durable worker started by Submit.
func (h *Handlers) handleDurableWS(
	ctx context.Context,
	cancel context.CancelFunc,
	connBase BaseRequest,
	conn *websocket.Conn,
	writer streamWriter,
) {
	started := false
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		frame, err := simplejson.NewJson(data)
		if err != nil {
			h.writeDurableStreamError(writer, "", ErrInvalidParam.WithMessage("invalid frame json"))
			continue
		}

		base := connBase
		base.Action = strings.TrimSpace(frame.Get("Action").MustString())
		if requestID := strings.TrimSpace(frame.Get("request_uuid").MustString()); requestID != "" {
			base.RequestUUID = requestID
		}
		if projectID := strings.TrimSpace(frame.Get("ProjectId").MustString()); projectID != "" {
			base.ProjectID = projectID
		}

		switch base.Action {
		case "SendCSAgentChat":
			if started {
				h.writeDurableCode(writer, "Conflict", "this connection already observes a turn")
				continue
			}
			input, apiErr := h.durableSubmitInput(ctx, base, frame)
			if apiErr != nil {
				h.writeDurableStreamError(writer, base.RequestUUID, apiErr)
				continue
			}
			submission, submitErr := h.turnCoordinator.Submit(ctx, input, nil)
			if submitErr != nil {
				h.writeDurableCoordinatorError(writer, submitErr)
				continue
			}
			started = true
			if err := writer.WriteEvent("meta", durableMetaFrame(submission.Turn, input.ClientTurnID, submission.Disposition)); err != nil {
				return
			}
			if err := h.subscribeDurableSocket(ctx, cancel, writer, submission.Turn, 0); err != nil {
				h.writeDurableCoordinatorError(writer, err)
				return
			}

		case "ResumeCSAgentTurn":
			if started {
				h.writeDurableCode(writer, "Conflict", "this connection already observes a turn")
				continue
			}
			if err := requireDurableV2(frame); err != nil {
				h.writeDurableStreamError(writer, base.RequestUUID, err)
				continue
			}
			lastSeq := frame.Get("LastSeq").MustInt64(-1)
			if lastSeq < 0 {
				h.writeDurableStreamError(writer, base.RequestUUID, ErrInvalidParam.WithMessage("LastSeq must be zero or greater"))
				continue
			}
			turn, lookupErr := h.findDurableTurn(ctx, base.Owner, frame)
			if lookupErr != nil {
				h.writeDurableCoordinatorError(writer, lookupErr)
				continue
			}
			started = true
			if err := writer.WriteEvent("meta", durableMetaFrame(turn, turn.ClientTurnID, turncoord.DispositionSubscribed)); err != nil {
				return
			}
			// A client that already persisted the terminal sequence may still ask
			// for reconciliation after losing its local status update. Re-send the
			// terminal event instead of leaving that socket open forever.
			if turn.Status.Terminal() && lastSeq >= turn.NextEventSeq-1 {
				lastSeq = max(turn.NextEventSeq-2, 0)
			}
			if err := h.subscribeDurableSocket(ctx, cancel, writer, turn, lastSeq); err != nil {
				h.writeDurableCoordinatorError(writer, err)
				return
			}

		case "CancelCSAgentTurn":
			if started {
				h.writeDurableCode(writer, "Conflict", "this connection already observes a turn")
				continue
			}
			if err := requireDurableV2(frame); err != nil {
				h.writeDurableStreamError(writer, base.RequestUUID, err)
				continue
			}
			turn, lookupErr := h.findDurableTurn(ctx, base.Owner, frame)
			if lookupErr != nil {
				h.writeDurableCoordinatorError(writer, lookupErr)
				continue
			}
			turn, abortErr := h.turnCoordinator.AbortTurn(ctx, base.Owner, turn.ID)
			if abortErr != nil {
				if errors.Is(abortErr, store.ErrLeaseHeld) || errors.Is(abortErr, store.ErrInvalidTurnState) {
					h.writeDurableCode(writer, "CancelPending", "the turn is already executing; its result must be reconciled before it can be cancelled")
					return
				}
				h.writeDurableCoordinatorError(writer, abortErr)
				continue
			}
			started = true
			if err := writer.WriteEvent("meta", durableMetaFrame(turn, turn.ClientTurnID, turncoord.DispositionReplayed)); err != nil {
				return
			}
			if err := h.subscribeDurableSocket(ctx, cancel, writer, turn, 0); err != nil {
				h.writeDurableCoordinatorError(writer, err)
				return
			}

		case "ConfirmCSAgentAction":
			sessionID := strings.TrimSpace(frame.Get("SessionId").MustString())
			turnID := strings.TrimSpace(frame.Get("TurnId").MustString())
			key := strings.TrimSpace(frame.Get("InteractionKey").MustString())
			if key == "" {
				key = strings.TrimSpace(frame.Get("ConfirmationId").MustString())
			}
			if sessionID == "" || turnID == "" || key == "" {
				h.writeDurableInteractionError(writer, turnID, key, ErrInvalidParam.WithMessage("SessionId, TurnId and InteractionKey are required"))
				continue
			}
			if err := h.validateDurableTurnSession(ctx, base.Owner, sessionID, turnID); err != nil {
				h.writeDurableInteractionError(writer, turnID, key, err)
				continue
			}
			overrides, overrideErr := overridesFromFrame(frame)
			if overrideErr != nil {
				h.writeDurableInteractionError(writer, turnID, key, ErrInvalidParam.WithMessage("%v", overrideErr))
				continue
			}
			if err := h.turnCoordinator.ResolveInteraction(ctx, base.Owner, turnID, key, turncoord.ConfirmationResponse{
				Confirmed: frame.Get("Confirmed").MustBool(false), Overrides: overrides,
			}); err != nil {
				h.writeDurableInteractionError(writer, turnID, key, err)
			}

		case "ConfirmCSAgentSelection":
			// Persistent selection semantics are not implemented by the durable
			// coordinator. Do not acknowledge and drop Value/Skipped.
			if strings.TrimSpace(frame.Get("TurnId").MustString()) == "" ||
				strings.TrimSpace(frame.Get("InteractionKey").MustString()) == "" {
				h.writeDurableStreamError(writer, base.RequestUUID, ErrInvalidParam.WithMessage("TurnId and InteractionKey are required"))
				continue
			}
			h.writeDurableStreamError(writer, base.RequestUUID, ErrInvalidParam.WithMessage("durable selections are not supported"))

		default:
			h.writeDurableStreamError(writer, base.RequestUUID, ErrInvalidParam.WithMessage("unsupported Action %s", base.Action))
		}
	}
}

func (h *Handlers) durableSubmitInput(ctx context.Context, base BaseRequest, frame *simplejson.Json) (turncoord.SubmitInput, *APIError) {
	protocol := frame.Get("ProtocolVersion").MustInt(0)
	if protocol != 0 && protocol != durableTurnProtocolVersion {
		return turncoord.SubmitInput{}, ErrInvalidParam.WithMessage("unsupported ProtocolVersion %d", protocol)
	}
	var clientConfirmForm, clientGuidedCreate, knowledgeOnly, feishuConsoleHandoff bool
	if features, err := frame.Get("Features").StringArray(); err == nil {
		for _, feature := range features {
			clientConfirmForm = clientConfirmForm || feature == featureConfirmForm
			clientGuidedCreate = clientGuidedCreate || feature == featureGuidedCreate
			knowledgeOnly = knowledgeOnly || feature == featureKnowledgeOnly
			feishuConsoleHandoff = feishuConsoleHandoff || feature == featureFeishuConsoleHandoff
		}
	}
	confirmForm := h.confirmFormEnabled && clientConfirmForm
	guidedCreate := confirmForm && h.guidedCreateEnabled && clientGuidedCreate
	sessionID := strings.TrimSpace(frame.Get("SessionId").MustString())
	message := strings.TrimSpace(frame.Get("Message").MustString())
	if sessionID == "" {
		return turncoord.SubmitInput{}, ErrInvalidParam.WithMessage("missing SessionId")
	}
	if message == "" {
		return turncoord.SubmitInput{}, ErrInvalidParam.WithMessage("missing Message")
	}
	if len([]rune(message)) > h.cfg.Agent.HTTP.MaxInputLength {
		return turncoord.SubmitInput{}, ErrInvalidParam.WithMessage("Message exceeds MaxInputLength")
	}
	session, err := h.sessions.GetByID(ctx, base.Owner, sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return turncoord.SubmitInput{}, ErrNotFound.WithMessage("session not found; create a new session explicitly")
		}
		return turncoord.SubmitInput{}, AsAPIError(err)
	}

	clientTurnID := strings.TrimSpace(frame.Get("ClientTurnId").MustString())
	if clientTurnID == "" {
		// V1 compatibility: the gateway request id becomes the stable key. A
		// retry must preserve request_uuid to reconcile the same turn.
		clientTurnID = base.RequestUUID
	}
	image := strings.TrimSpace(frame.Get("Image").MustString())
	var imageContext, imageDigest string
	if image != "" {
		imageDigest, err = durableImageDigest(image, h.cfg.Agent.OCR.MaxBytes)
		if err != nil {
			return turncoord.SubmitInput{}, ErrInvalidParam.WithMessage("invalid Image: %v", err)
		}
		if h.ocrClient != nil {
			imageContext, err = h.processOCR(ctx, base.RequestUUID, image)
			if err != nil {
				return turncoord.SubmitInput{}, ErrInvalidParam.WithMessage("invalid Image: %v", err)
			}
		} else {
			log.Printf("warning: Image provided but OCR not configured (request %s)", base.RequestUUID)
		}
	}
	userContext, err := h.buildUserContext(base)
	if err != nil {
		return turncoord.SubmitInput{}, AsAPIError(err)
	}
	if session.Title == nil {
		if title := deriveSessionTitle(message); title != "" {
			if err := h.sessions.SetTitleIfEmpty(ctx, base.Owner, sessionID, title); err != nil {
				log.Printf("warning: session %s title derivation failed (non-fatal): %v", sessionID, err)
			}
		}
	}
	requestID := base.RequestUUID
	model := h.cfg.Agent.LLM.Model
	return turncoord.SubmitInput{
		Owner: base.Owner, SessionID: sessionID, ClientTurnID: clientTurnID,
		Message: message, RequestUUID: &requestID, AssistantModel: &model,
		ImageContext: imageContext, ImageDigest: imageDigest, UserContext: userContext,
		ConfirmForm: confirmForm, GuidedCreate: guidedCreate, KnowledgeOnly: knowledgeOnly,
		FeishuConsoleHandoff: feishuConsoleHandoff,
	}, nil
}

// durableImageDigest validates the same image contract as OCR, then hashes the
// decoded bytes. The data-URL spelling and OCR output are deliberately absent
// from idempotency identity.
func durableImageDigest(dataURL string, maxBytes int) (string, error) {
	if _, err := ocr.ValidateImageDataURL(dataURL, maxBytes); err != nil {
		return "", err
	}
	const marker = ";base64,"
	index := strings.Index(dataURL, marker)
	if index < 0 {
		return "", fmt.Errorf("expected base64 image")
	}
	decoded, err := base64.StdEncoding.DecodeString(dataURL[index+len(marker):])
	if err != nil {
		return "", fmt.Errorf("decode image: %w", err)
	}
	return turncoord.StableImageDigest(decoded), nil
}

func requireDurableV2(frame *simplejson.Json) *APIError {
	if version := frame.Get("ProtocolVersion").MustInt(0); version != durableTurnProtocolVersion {
		return ErrInvalidParam.WithMessage("ProtocolVersion 2 is required")
	}
	return nil
}

func (h *Handlers) findDurableTurn(ctx context.Context, owner store.Owner, frame *simplejson.Json) (store.Turn, error) {
	sessionID := strings.TrimSpace(frame.Get("SessionId").MustString())
	if sessionID == "" {
		return store.Turn{}, fmt.Errorf("%w: missing SessionId", store.ErrInvalidArgument)
	}
	turnID := strings.TrimSpace(frame.Get("TurnId").MustString())
	clientTurnID := strings.TrimSpace(frame.Get("ClientTurnId").MustString())
	if turnID == "" && clientTurnID == "" {
		return store.Turn{}, fmt.Errorf("%w: TurnId or ClientTurnId is required", store.ErrInvalidArgument)
	}
	var (
		turn store.Turn
		err  error
	)
	if turnID != "" {
		turn, err = h.turnCoordinator.GetTurn(ctx, owner, turnID)
	} else {
		turn, err = h.turnCoordinator.FindTurnByClientID(ctx, owner, sessionID, clientTurnID)
	}
	if err != nil {
		return store.Turn{}, err
	}
	if turn.SessionID != sessionID || (clientTurnID != "" && turn.ClientTurnID != clientTurnID) {
		return store.Turn{}, store.ErrTurnNotFound
	}
	return turn, nil
}

func (h *Handlers) validateDurableTurnSession(ctx context.Context, owner store.Owner, sessionID, turnID string) error {
	turn, err := h.turnCoordinator.GetTurn(ctx, owner, turnID)
	if err != nil {
		return err
	}
	if turn.SessionID != sessionID {
		// Keep wrong-session and unknown-turn responses indistinguishable. More
		// importantly, a stale tab can never approve a card from another chat.
		return store.ErrTurnNotFound
	}
	return nil
}

func (h *Handlers) subscribeDurableSocket(ctx context.Context, cancel context.CancelFunc, writer streamWriter, turn store.Turn, lastSeq int64) error {
	return h.turnCoordinator.Subscribe(ctx, turn.Owner, turn.ID, lastSeq, func(event turncoord.Event) error {
		eventName, frame, err := coordinatorStreamEvent(turn, event)
		if err != nil {
			return err
		}
		if err := writer.WriteEvent(eventName, frame); err != nil {
			return err
		}
		if durableEventEndsSocket(event) {
			cancel()
		}
		return nil
	})
}

func durableEventEndsSocket(event turncoord.Event) bool {
	if !event.Provisional {
		return true
	}
	switch event.Type {
	case "turn.failed", "turn.reconciled", "turn.lease_released":
		return true
	default:
		return false
	}
}

func durableMetaFrame(turn store.Turn, clientTurnID string, disposition turncoord.Disposition) map[string]any {
	return map[string]any{
		"TurnId": turn.ID, "SessionId": turn.SessionID, "ClientTurnId": clientTurnID,
		// ServerStatus is diagnostic only. Do not put the database status in the
		// frontend-consumed Status field: on Resume of an already committed turn,
		// Status=committed would make the client close before the persisted done
		// event (and its answer) is replayed.
		"ServerStatus": string(turn.Status), "Disposition": string(disposition),
	}
}

// writeDurableStreamError reports a rejected frame. requestID may be empty for a
// frame that failed to parse before one was known; everywhere else it is what the
// client sees, and it is the only handle an operator has on the cause an internal
// error no longer prints to the user.
func (h *Handlers) writeDurableStreamError(writer streamWriter, requestID string, err error) {
	apiErr := AsAPIError(err)
	logInternalCause("durable stream", requestID, apiErr)
	h.writeDurableCode(writer, apiErr.Code, apiErr.Message)
}

func (h *Handlers) writeDurableCoordinatorError(writer streamWriter, err error) {
	code, message := durableCoordinatorError(err)
	h.writeDurableCode(writer, code, message)
}

type durableInteractionErrorEvent struct {
	Code           string `json:"Code"`
	Message        string `json:"Message"`
	TurnID         string `json:"TurnId,omitempty"`
	InteractionKey string `json:"InteractionKey,omitempty"`
}

// writeDurableInteractionError reports a rejected confirmation submission
// without ending the observed turn. The durable interaction remains pending,
// so the same card must stay editable and may be submitted again.
func (h *Handlers) writeDurableInteractionError(writer streamWriter, turnID, key string, err error) {
	code, message := durableCoordinatorError(err)
	_ = writer.WriteEvent("interaction_error", durableInteractionErrorEvent{
		Code: code, Message: message, TurnID: turnID, InteractionKey: key,
	})
}

func durableCoordinatorError(err error) (string, string) {
	code, message := "InternalError", "durable turn request failed"
	switch {
	case errors.Is(err, store.ErrInvalidArgument):
		code, message = "InvalidParam", strings.TrimPrefix(err.Error(), store.ErrInvalidArgument.Error()+": ")
	case errors.Is(err, store.ErrIdempotencyConflict):
		code, message = "Conflict", "ClientTurnId was already used for a different request"
	case errors.Is(err, store.ErrTurnNotFound), errors.Is(err, store.ErrConversationNotFound), errors.Is(err, sql.ErrNoRows):
		code, message = "NotFound", "turn or session not found"
	case errors.Is(err, store.ErrLeaseHeld), errors.Is(err, store.ErrTurnOutOfOrder):
		code, message = "TurnBusy", "an earlier turn is still being reconciled"
	case errors.Is(err, store.ErrInteractionExpired):
		code, message = "InvalidParam", "the confirmation has expired"
	case errors.Is(err, store.ErrInteractionConflict), errors.Is(err, store.ErrInvalidTurnState):
		code, message = "Conflict", "the turn state changed; resume it before retrying"
	}
	return code, message
}

func (h *Handlers) writeDurableCode(writer streamWriter, code, message string) {
	_ = writer.WriteEvent("error", streamErrorEvent{Code: code, Message: message})
}
