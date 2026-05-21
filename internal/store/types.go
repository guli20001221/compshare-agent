package store

import (
	"context"
	"encoding/json"
	"time"
)

// Owner identifies a user by their organization hierarchy.
type Owner struct {
	TopOrganizationID uint32
	OrganizationID    uint32
}

// Session represents a conversation session.
type Session struct {
	ID                string
	TopOrganizationID uint32
	OrganizationID    uint32
	Title             *string
	Context           json.RawMessage
	MessageCount      int
	Pinned            bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Message represents a single message in a session.
type Message struct {
	ID           string
	SessionID    string
	RequestUUID  string
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
	BumpUpdatedAtAndIncCount(ctx context.Context, sessionID string, delta int) error
}

// MessageStore manages messages within sessions.
type MessageStore interface {
	Append(ctx context.Context, m Message) error
	UpdateAssistant(ctx context.Context, msgID string, patch AssistantPatch) error
	ListBySession(ctx context.Context, sessionID string, limit int, cursor string) ([]Message, string, error)
	GetWithOwnerCheck(ctx context.Context, owner Owner, msgID string) (Message, error)
}

// FeedbackStore manages user feedback on messages.
type FeedbackStore interface {
	Insert(ctx context.Context, msgID, rating, comment string) (string, error)
}
