// Package server hosts the console-deployment WebSocket entry point.
//
// Protocol (v1):
//
//   - Client → Server messages are JSON objects matching ClientMessage.
//     The ONLY messages clients send: "user_message", "confirm_response",
//     "ping".
//   - Server → Client messages are JSON objects matching ServerMessage.
//
//   SECURITY: ClientMessage MUST NOT carry any tenant identity (top_org_id,
//   organization_id, user_id, jwt token). Identity is established BEFORE
//   the WS reader loop runs — by the gateway in production (URL params or
//   headers injected by the console gateway) or by JWT verification in
//   future deployment modes. A client cannot impersonate another tenant
//   because every message it sends is implicitly scoped to its connection's
//   already-established identity.
package server

// ProtocolVersion is the wire-format version. Bump on any breaking change to
// ClientMessage / ServerMessage; non-breaking additions (new optional field,
// new message Type) DO NOT bump.
const ProtocolVersion = "v1"

// Client → Server message types.
const (
	ClientMsgUserMessage     = "user_message"
	ClientMsgConfirmResponse = "confirm_response"
	ClientMsgPing            = "ping"
)

// ClientMessage is every JSON frame the server can ever receive on a WS
// connection. Fields are optional; the message Type selects which subset
// is read. See SECURITY note in the package doc.
type ClientMessage struct {
	Type string `json:"type"`

	// RequestUUID is the client-generated idempotency / correlation key
	// for a single user turn. The server stamps it onto every response
	// frame and onto the persisted trace + message rows.
	RequestUUID string `json:"request_uuid,omitempty"`

	// Text is the user-typed message body. Required when Type=user_message.
	Text string `json:"text,omitempty"`

	// Confirmed is the client's answer to a prior ServerMsgConfirmRequired.
	// Required when Type=confirm_response.
	Confirmed bool `json:"confirmed,omitempty"`
}

// Server → Client message types.
const (
	ServerMsgReady           = "ready"
	ServerMsgToolCall        = "tool_call"
	ServerMsgToolResult      = "tool_result"
	ServerMsgConfirmRequired = "confirm_required"
	ServerMsgAnswerDelta     = "answer_delta"
	ServerMsgAnswerFinal     = "answer_final"
	ServerMsgError           = "error"
	ServerMsgDone            = "done"
	ServerMsgPong            = "pong"
)

// Error codes (stable across protocol versions). Clients should switch on
// Code, not Message — Message is human-readable and may be localized.
const (
	ErrCodeUnauthorized        = "unauthorized"
	ErrCodeRateLimited         = "rate_limited"
	ErrCodeUpstreamUnavailable = "upstream_unavailable"
	ErrCodeInternal            = "internal_error"
	ErrCodeProtocolViolation   = "protocol_violation"
)

// ServerMessage is every JSON frame the server can send. Type selects which
// subset of fields is meaningful.
type ServerMessage struct {
	Type string `json:"type"`

	// RequestUUID is echoed back from the client message that prompted
	// this frame. Empty for connection-scoped events (ready / pong).
	RequestUUID string `json:"request_uuid,omitempty"`

	// SessionID is sent once in the "ready" frame. Server-generated UUID
	// identifying the WS connection. Useful for cross-message debugging.
	SessionID string `json:"session_id,omitempty"`

	// Suggestions accompanies the "ready" frame: a small list of opener
	// prompts the engine.Init produced.
	Suggestions []Suggestion `json:"suggestions,omitempty"`

	// Action / Args / OK populate tool_call, tool_result, confirm_required.
	Action string         `json:"action,omitempty"`
	Args   map[string]any `json:"args,omitempty"`
	OK     *bool          `json:"ok,omitempty"`

	// Text carries the assistant reply (answer_final) or a chunk
	// (answer_delta, A8 — not implemented in v1).
	Text string `json:"text,omitempty"`

	// Code / Message populate the "error" frame.
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// Suggestion is a startup opener prompt forwarded from engine.Init.
type Suggestion struct {
	Text string `json:"text"`
}
