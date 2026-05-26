package httpapi

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Dispatch routes an incoming gateway request to the appropriate Action handler.
// Non-SSE handlers go through writeResult; Chat is handled via SSE in handleChat.
func (h *Handlers) Dispatch(c *gin.Context) {
	raw, base, err := ParseBaseRequest(c)
	if err != nil {
		h.writeError(c, base.Action, base.RequestUUID, err)
		return
	}

	switch base.Action {
	case "GetCSAgentMeta":
		data, err := h.handleGetMeta(c, base, raw)
		h.writeResult(c, base, data, err)
	case "CreateCSAgentSession":
		data, err := h.handleCreateSession(c, base, raw)
		h.writeResult(c, base, data, err)
	case "GetCSAgentSession":
		data, err := h.handleGetSession(c, base, raw)
		h.writeResult(c, base, data, err)
	case "SendCSAgentFeedback":
		data, err := h.handleFeedback(c, base, raw)
		h.writeResult(c, base, data, err)
	case "SendCSAgentChat":
		h.handleChat(c, base, raw)
	default:
		h.writeError(c, base.Action, base.RequestUUID, ErrInvalidParam.WithMessage("unsupported Action %s", base.Action))
	}
}

// writeResult writes a successful JSON response with UCloud-standard envelope
// fields (Action / RetCode / Message / RequestId) and the handler-supplied
// data flattened to top-level — there is no nested Data wrapper. Envelope
// keys take precedence over any colliding data field names.
func (h *Handlers) writeResult(c *gin.Context, base BaseRequest, data any, err error) {
	if err != nil {
		h.writeError(c, base.Action, base.RequestUUID, err)
		return
	}
	body, mErr := flattenEnvelope(base.Action, base.RequestUUID, 0, "", data)
	if mErr != nil {
		h.writeError(c, base.Action, base.RequestUUID, mErr)
		return
	}
	c.JSON(http.StatusOK, body)
}

// writeError converts err to an APIError and responds with the appropriate HTTP status.
// sql.ErrNoRows is canonicalized to ErrNotFound. Error responses carry only
// the envelope fields (Action / RetCode / Message / RequestId), no data payload.
// action may be empty when the request failed to parse before Action was known.
func (h *Handlers) writeError(c *gin.Context, action, requestID string, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	apiErr := AsAPIError(err)
	body := gin.H{
		"RetCode":   apiErr.RetCode,
		"Message":   apiErr.Message,
		"RequestId": requestID,
	}
	if action != "" {
		body["Action"] = action
	}
	c.JSON(apiErr.Status, body)
}

// flattenEnvelope marshals data and merges its top-level JSON fields with the
// UCloud-standard envelope (Action / RetCode / Message / RequestId). Envelope
// keys win on collision. Action is omitted when empty.
// Returns an error only if data fails to marshal/unmarshal.
func flattenEnvelope(action, requestID string, retCode int, message string, data any) (map[string]any, error) {
	body := map[string]any{}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
	}
	if action != "" {
		body["Action"] = action
	}
	body["RetCode"] = retCode
	if message != "" {
		body["Message"] = message
	}
	body["RequestId"] = requestID
	return body, nil
}
