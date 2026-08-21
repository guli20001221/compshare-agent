package httpapi

import (
	"errors"

	"github.com/bitly/go-simplejson"
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
	// POST confirmations remain boolean-only; editable form overrides travel on
	// the WebSocket that owns the in-flight turn.
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
