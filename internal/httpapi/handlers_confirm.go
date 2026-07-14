package httpapi

import (
	"errors"
	"strings"

	"github.com/bitly/go-simplejson"
	"github.com/compshare-agent/internal/store"
	"github.com/compshare-agent/internal/turncoord"
	"github.com/gin-gonic/gin"
)

type confirmResponse struct {
	SessionID      string `json:"SessionId"`
	ConfirmationID string `json:"ConfirmationId"`
	Accepted       bool   `json:"Accepted"`
}

func (h *Handlers) handleConfirm(c *gin.Context, base BaseRequest, raw *simplejson.Json) (any, error) {
	sessionID := raw.Get("SessionId").MustString()
	if sessionID == "" {
		return nil, ErrInvalidParam.WithMessage("missing SessionId")
	}
	confirmationID := raw.Get("ConfirmationId").MustString()
	if confirmationID == "" {
		return nil, ErrInvalidParam.WithMessage("missing ConfirmationId")
	}
	confirmed := raw.Get("Confirmed").MustBool(false)
	if h.turnCoordinator != nil {
		turnID := strings.TrimSpace(raw.Get("TurnId").MustString())
		interactionKey := strings.TrimSpace(raw.Get("InteractionKey").MustString())
		if interactionKey == "" {
			interactionKey = confirmationID
		}
		if turnID == "" || interactionKey == "" {
			return nil, ErrInvalidParam.WithMessage("TurnId and InteractionKey are required")
		}
		if err := h.validateDurableTurnSession(c.Request.Context(), base.Owner, sessionID, turnID); err != nil {
			return nil, ErrNotFound.WithMessage("confirmation turn not found in this session")
		}
		if _, exists := raw.CheckGet("Overrides"); exists {
			return nil, ErrInvalidParam.WithMessage("editable durable confirmations are not supported")
		}
		if err := h.turnCoordinator.ResolveInteraction(c.Request.Context(), base.Owner, turnID, interactionKey, turncoord.ConfirmationResponse{Confirmed: confirmed}); err != nil {
			switch {
			case errors.Is(err, store.ErrInteractionExpired):
				return nil, ErrInvalidParam.WithMessage("confirmation has expired")
			default:
				return nil, ErrNotFound.WithMessage("confirmation not found or already resolved")
			}
		}
		return confirmResponse{SessionID: sessionID, ConfirmationID: interactionKey, Accepted: confirmed}, nil
	}

	// Boolean-only on the POST path: the editable confirm form is WS-only
	// (Features opt-in lives on the WS SendCSAgentChat frame), so no Overrides
	// are accepted here.
	err := h.confirmBroker.Resolve(confirmationID, sessionID, base.Owner, ConfirmDecision{Confirmed: confirmed})
	if err != nil {
		if errors.Is(err, ErrConfirmationOwner) {
			return nil, ErrForbidden.WithMessage("confirmation does not belong to this owner")
		}
		return nil, ErrNotFound.WithMessage("confirmation %s not found or already resolved", confirmationID)
	}

	return confirmResponse{
		SessionID:      sessionID,
		ConfirmationID: confirmationID,
		Accepted:       confirmed,
	}, nil
}
