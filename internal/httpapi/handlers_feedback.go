package httpapi

import (
	"github.com/bitly/go-simplejson"
	"github.com/gin-gonic/gin"
)

// feedbackData is the Data payload for a successful Feedback response.
type feedbackData struct {
	FeedbackID string `json:"FeedbackId"`
}

// handleFeedback records user feedback (thumbs up / thumbs down) for an assistant message.
// Required: MessageId (non-empty), Rating ("Up" or "Down").
// Optional: Comment (free text).
func (h *Handlers) handleFeedback(c *gin.Context, base BaseRequest, raw *simplejson.Json) (any, error) {
	messageID := raw.Get("MessageId").MustString()
	if messageID == "" {
		return nil, ErrInvalidParam.WithMessage("missing MessageId")
	}
	rating := raw.Get("Rating").MustString()
	if rating != "Up" && rating != "Down" {
		return nil, ErrInvalidParam.WithMessage("Rating must be Up or Down")
	}
	comment := raw.Get("Comment").MustString()

	// Verify the message belongs to the requesting owner before recording feedback.
	if _, err := h.messages.GetWithOwnerCheck(c.Request.Context(), base.Owner, messageID); err != nil {
		return nil, err
	}

	id, err := h.feedback.Insert(c.Request.Context(), messageID, rating, comment)
	if err != nil {
		return nil, err
	}
	return feedbackData{FeedbackID: id}, nil
}
