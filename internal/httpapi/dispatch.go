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
		h.writeError(c, "", err)
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
		h.writeError(c, base.RequestUUID, ErrInvalidParam.WithMessage("unsupported Action %s", base.Action))
	}
}

// writeResult writes a successful JSON response with envelope fields
// (RequestId / Code / Message) and the handler-supplied data flattened to
// top-level — there is no nested Data wrapper. Envelope fields take
// precedence over any colliding data field names.
func (h *Handlers) writeResult(c *gin.Context, base BaseRequest, data any, err error) {
	if err != nil {
		h.writeError(c, base.RequestUUID, err)
		return
	}
	body, mErr := flattenEnvelope(base.RequestUUID, "Success", "", data)
	if mErr != nil {
		h.writeError(c, base.RequestUUID, mErr)
		return
	}
	c.JSON(http.StatusOK, body)
}

// writeError converts err to an APIError and responds with the appropriate HTTP status.
// sql.ErrNoRows is canonicalized to ErrNotFound. Error responses carry only
// the envelope fields (no data payload).
func (h *Handlers) writeError(c *gin.Context, requestID string, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	apiErr := AsAPIError(err)
	c.JSON(apiErr.Status, gin.H{
		"RequestId": requestID,
		"Code":      apiErr.Code,
		"Message":   apiErr.Message,
	})
}

// flattenEnvelope marshals data and merges its top-level JSON fields with
// the envelope (RequestId/Code/Message). Envelope keys win on collision.
// Returns an error only if data fails to marshal/unmarshal.
func flattenEnvelope(requestID, code, message string, data any) (map[string]any, error) {
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
	body["RequestId"] = requestID
	body["Code"] = code
	body["Message"] = message
	return body, nil
}
