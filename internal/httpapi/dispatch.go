package httpapi

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Dispatch routes an incoming gateway request to the appropriate Action handler.
// Non-SSE handlers go through writeResult; Chat is handled via SSE in handleChat.
func (h *Handlers) Dispatch(c *gin.Context) {
	raw, base, err := ParseBaseRequest(c)
	if err != nil {
		h.writeError(c, "", err)
		return
	}

	switch base.Action {
	case "GetMeta":
		data, err := h.handleGetMeta(c, base, raw)
		h.writeResult(c, base, data, err)
	case "CreateSession":
		data, err := h.handleCreateSession(c, base, raw)
		h.writeResult(c, base, data, err)
	case "GetSession":
		data, err := h.handleGetSession(c, base, raw)
		h.writeResult(c, base, data, err)
	case "Feedback":
		data, err := h.handleFeedback(c, base, raw)
		h.writeResult(c, base, data, err)
	case "Chat":
		h.handleChat(c, base, raw)
	default:
		h.writeError(c, base.RequestUUID, ErrInvalidParam.WithMessage("unsupported Action %s", base.Action))
	}
}

// writeResult writes a successful JSON response envelope.
func (h *Handlers) writeResult(c *gin.Context, base BaseRequest, data any, err error) {
	if err != nil {
		h.writeError(c, base.RequestUUID, err)
		return
	}
	c.JSON(http.StatusOK, Response{
		RequestID: base.RequestUUID,
		Code:      "Success",
		Message:   "",
		Data:      data,
	})
}

// writeError converts err to an APIError and responds with the appropriate HTTP status.
// sql.ErrNoRows is canonicalized to ErrNotFound.
func (h *Handlers) writeError(c *gin.Context, requestID string, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	apiErr := AsAPIError(err)
	c.JSON(apiErr.Status, Response{
		RequestID: requestID,
		Code:      apiErr.Code,
		Message:   apiErr.Message,
		Data:      nil,
	})
}
