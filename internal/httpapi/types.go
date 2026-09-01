package httpapi

import "time"

// Response is the UCloud-standard success envelope returned for non-SSE
// gateway responses. RetCode is 0 and Action echoes the request's Action when
// known. Error envelopes are written by writeError and additionally carry the
// stable string Code; Action-specific success fields are flattened onto this
// same JSON object with no nested Data wrapper. See dispatch.go.
type Response struct {
	Action    string `json:"Action,omitempty"`
	RetCode   int    `json:"RetCode"`
	Message   string `json:"Message,omitempty"`
	RequestID string `json:"RequestId"`
}

// MessageDTO is the wire representation of a conversation message returned
// in GetSession responses.
type MessageDTO struct {
	MessageID string    `json:"MessageId"`
	Role      string    `json:"Role"`
	Content   string    `json:"Content"`
	Status    string    `json:"Status"`
	CreatedAt time.Time `json:"CreatedAt"`
}
