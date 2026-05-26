package httpapi

import "time"

// Response is the UCloud-standard envelope returned for all non-SSE gateway
// responses. RetCode is 0 on success and a non-zero integer (see APIError) on
// failure. Action echoes the request's Action when known. Action-specific
// fields are flattened onto the same JSON object alongside these envelope
// fields — there is no nested Data wrapper. See dispatch.go's
// flattenEnvelope for how the merging happens.
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
