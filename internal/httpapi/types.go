package httpapi

import "time"

// Response is the envelope returned for all non-SSE gateway responses.
// Action-specific fields are flattened onto the same JSON object alongside
// these envelope fields — there is no nested Data wrapper. See dispatch.go's
// flattenEnvelope for how the merging happens.
type Response struct {
	RequestID string `json:"RequestId"`
	Code      string `json:"Code"`
	Message   string `json:"Message"`
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
