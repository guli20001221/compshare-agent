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

	// ListByOwner returns up to limit of the owner's sessions, most recently
	// active first (ORDER BY updated_at DESC), excluding soft-deleted rows. It
	// backs the history sidebar; messages are not loaded. limit must be >= 1;
	// a limit < 1 yields an empty result (callers are expected to clamp).
	ListByOwner(ctx context.Context, owner Owner, limit int) ([]Session, error)

	// SetTitleIfEmpty sets sessions.title to title only when the row's title is
	// currently NULL, preserving an explicit client-set title. Owner-scoped +
	// deleted_at IS NULL. 0 rows affected (title already set, or row missing) is
	// not an error — this is a best-effort first-turn derivation that must never
	// fail the chat turn.
	SetTitleIfEmpty(ctx context.Context, owner Owner, sessionID string, title string) error
}

// MessageStore manages messages within sessions.
type MessageStore interface {
	Append(ctx context.Context, m Message) error
	UpdateAssistant(ctx context.Context, owner Owner, msgID string, patch AssistantPatch) error
	ListBySession(ctx context.Context, sessionID string, limit int, cursor string) ([]Message, string, error)
	GetWithOwnerCheck(ctx context.Context, owner Owner, msgID string) (Message, error)
}

// CommittedTailMessageStore is the history contract used by the durable turn
// protocol. It is intentionally separate from MessageStore so legacy/test
// stores do not silently inherit unsafe head-page semantics. Implementations
// must return only protocol-committed complete user/assistant pairs from the
// newest turnLimit turns, ordered oldest-to-newest within that tail. Protocol
// commitment includes both committed v2 chat_turns and strict legacy pairs:
// the same non-empty request_uuid, exactly one ok user, and exactly one ok
// assistant in the owner-scoped session. Half/error/duplicate/null legacy
// groups must never enter model history.
type CommittedTailMessageStore interface {
	ListCommittedTail(ctx context.Context, owner Owner, sessionID string, turnLimit int) ([]Message, error)
}

// CommittedPageMessageStore is the user-visible history contract. Like the
// engine tail reader above, it exposes only complete protocol-committed pairs,
// but adds an opaque pair cursor for GetSession pagination. A page boundary
// must never split a user/assistant pair.
type CommittedPageMessageStore interface {
	ListCommittedBySession(ctx context.Context, owner Owner, sessionID string, limit int, cursor string) ([]Message, string, int, error)
}

// FeedbackStore manages user feedback on messages.
type FeedbackStore interface {
	Insert(ctx context.Context, msgID, rating, comment string) (string, error)
}
