package httpapi

import (
	"encoding/json"

	"github.com/bitly/go-simplejson"
	"github.com/gin-gonic/gin"
)

// createSessionData is the Data payload for a successful CreateSession response.
type createSessionData struct {
	SessionID string  `json:"SessionId"`
	Title     *string `json:"Title"`
	CreatedAt any     `json:"CreatedAt"`
}

// getSessionData is the Data payload for a successful GetSession response.
type getSessionData struct {
	SessionID    string       `json:"SessionId"`
	Title        *string      `json:"Title"`
	MessageCount int          `json:"MessageCount"`
	CreatedAt    any          `json:"CreatedAt"`
	UpdatedAt    any          `json:"UpdatedAt"`
	Messages     []MessageDTO `json:"Messages"`
	NextCursor   string       `json:"NextCursor,omitempty"`
}

// handleCreateSession creates a new session for the authenticated owner.
// Optional fields: Title (string), Context (raw JSON object).
func (h *Handlers) handleCreateSession(c *gin.Context, base BaseRequest, raw *simplejson.Json) (any, error) {
	title := optionalString(raw, "Title")
	ctxJSON, err := optionalJSON(raw, "Context")
	if err != nil {
		return nil, ErrInvalidParam.WithMessage("invalid Context")
	}
	sess, err := h.sessions.Create(c.Request.Context(), base.Owner, title, ctxJSON)
	if err != nil {
		return nil, err
	}
	return createSessionData{
		SessionID: sess.ID,
		Title:     sess.Title,
		CreatedAt: sess.CreatedAt,
	}, nil
}

// handleGetSession retrieves session metadata and a page of messages.
// Required: SessionId. Optional: Limit (1–100, default 50), Cursor (opaque).
func (h *Handlers) handleGetSession(c *gin.Context, base BaseRequest, raw *simplejson.Json) (any, error) {
	sessionID := raw.Get("SessionId").MustString()
	if sessionID == "" {
		return nil, ErrInvalidParam.WithMessage("missing SessionId")
	}
	limit := raw.Get("Limit").MustInt(50)
	if limit < 1 || limit > 100 {
		return nil, ErrInvalidParam.WithMessage("Limit must be between 1 and 100")
	}
	cursor := raw.Get("Cursor").MustString()

	sess, err := h.sessions.GetByID(c.Request.Context(), base.Owner, sessionID)
	if err != nil {
		return nil, err
	}
	messages, nextCursor, err := h.messages.ListBySession(c.Request.Context(), sessionID, limit, cursor)
	if err != nil {
		return nil, ErrInvalidParam.WithMessage("invalid Cursor")
	}

	dtos := make([]MessageDTO, 0, len(messages))
	for _, msg := range messages {
		dtos = append(dtos, MessageDTO{
			MessageID: msg.ID,
			Role:      msg.Role,
			Content:   msg.Content,
			Status:    msg.Status,
			CreatedAt: msg.CreatedAt,
		})
	}
	return getSessionData{
		SessionID:    sess.ID,
		Title:        sess.Title,
		MessageCount: sess.MessageCount,
		CreatedAt:    sess.CreatedAt,
		UpdatedAt:    sess.UpdatedAt,
		Messages:     dtos,
		NextCursor:   nextCursor,
	}, nil
}

// optionalString returns nil if the key is absent or empty, otherwise a pointer to the string value.
func optionalString(raw *simplejson.Json, key string) *string {
	s := raw.Get(key).MustString()
	if s == "" {
		return nil
	}
	return &s
}

// optionalJSON returns nil if the key is absent or empty; otherwise validates and
// returns the value as json.RawMessage. Returns an error if the value is not valid JSON.
func optionalJSON(raw *simplejson.Json, key string) (json.RawMessage, error) {
	s := raw.Get(key).MustString()
	if s == "" {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, err
	}
	return json.RawMessage(s), nil
}
