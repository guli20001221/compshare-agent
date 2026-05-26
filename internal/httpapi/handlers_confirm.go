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

func (h *Handlers) handleConfirm(_ *gin.Context, base BaseRequest, raw *simplejson.Json) (any, error) {
	sessionID := raw.Get("SessionId").MustString()
	if sessionID == "" {
		return nil, ErrInvalidParam.WithMessage("missing SessionId")
	}
	confirmationID := raw.Get("ConfirmationId").MustString()
	if confirmationID == "" {
		return nil, ErrInvalidParam.WithMessage("missing ConfirmationId")
	}
	confirmed := raw.Get("Confirmed").MustBool(false)

	err := h.confirmBroker.Resolve(confirmationID, sessionID, base.Owner, confirmed)
	if err != nil {
		if errors.Is(err, ErrConfirmationOwner) {
			return nil, ErrForbidden.WithMessage("confirmation does not belong to this session")
		}
		return nil, ErrNotFound.WithMessage("confirmation %s not found or already resolved", confirmationID)
	}

	return confirmResponse{
		SessionID:      sessionID,
		ConfirmationID: confirmationID,
		Accepted:       confirmed,
	}, nil
}
