package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrStaleWrite is returned by SessionStore.UpdateContext when the supplied
// expectedVersion does not match the row's current context_version (or the
// row no longer satisfies owner / deleted_at constraints). Callers should
// log + continue: the in-memory engine state is still authoritative for
// the current turn; the next turn will re-hydrate from the winning row.
var ErrStaleWrite = errors.New("store: stale context_version on UpdateContext")

// Owner identifies a user by their organization hierarchy.
type Owner struct {
	TopOrganizationID uint32
	OrganizationID    uint32
}

// Session represents a conversation session.
//
// Context holds the persisted envelope JSON for the agent SessionState +
// opaque client_context (see internal/engine/session_state.go).
// ContextVersion backs the sessions.context_version column and is used by
// UpdateContext for optimistic concurrency control.
type Session struct {
	ID                string
	TopOrganizationID uint32
	OrganizationID    uint32
	Title             *string
	Context           json.RawMessage
	ContextVersion    int
	MessageCount      int
	Pinned            bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Message represents a single message in a session.
type Message struct {
	ID           string
	SessionID    string
	RequestUUID  *string
	Role         string
	Content      string
	Status       string
	ErrorCode    *string
	Model        *string
	InputTokens  *int
	OutputTokens *int
	TTFTMs       *int
	LatencyMs    *int
	Metadata     json.RawMessage
	CreatedAt    time.Time
}

// AssistantPatch holds fields to update on an assistant message after LLM response.
type AssistantPatch struct {
	Content      string
	Status       string
	ErrorCode    *string
	InputTokens  *int
	OutputTokens *int
	TTFTMs       *int
	LatencyMs    *int
}

// SessionStore manages session lifecycle.
type SessionStore interface {
	Create(ctx context.Context, owner Owner, title *string, ctxJSON json.RawMessage) (Session, error)
	GetByID(ctx context.Context, owner Owner, sessionID string) (Session, error)
	BumpUpdatedAtAndIncCount(ctx context.Context, owner Owner, sessionID string, delta int) error

	// UpdateContext atomically writes ctxJSON into sessions.context and
	// increments sessions.context_version, but only if the row's current
	// context_version equals expectedVersion. On version mismatch (or row
	// missing / deleted / owner mismatch) it returns ErrStaleWrite without
	// writing. On success it returns the new context_version value
	// (expectedVersion + 1).
	UpdateContext(
		ctx context.Context,
		owner Owner,
		sessionID string,
		ctxJSON json.RawMessage,
		expectedVersion int,
	) (newVersion int, err error)
}

// MessageStore manages messages within sessions.
type MessageStore interface {
	Append(ctx context.Context, m Message) error
	UpdateAssistant(ctx context.Context, owner Owner, msgID string, patch AssistantPatch) error
	ListBySession(ctx context.Context, sessionID string, limit int, cursor string) ([]Message, string, error)
	GetWithOwnerCheck(ctx context.Context, owner Owner, msgID string) (Message, error)
}

// FeedbackStore manages user feedback on messages.
type FeedbackStore interface {
	Insert(ctx context.Context, msgID, rating, comment string) (string, error)
}
