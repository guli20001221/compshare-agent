package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// MySQLSessionStore implements SessionStore using MySQL.
type MySQLSessionStore struct {
	db *sql.DB
}

// NewSessionStore returns a new MySQLSessionStore.
func NewSessionStore(db *sql.DB) *MySQLSessionStore {
	return &MySQLSessionStore{db: db}
}

// Create inserts a new session and returns it by calling GetByID.
func (s *MySQLSessionStore) Create(ctx context.Context, owner Owner, title *string, ctxJSON json.RawMessage) (Session, error) {
	id := uuid.NewString()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO sessions (id, top_organization_id, organization_id, title, context)
VALUES (?, ?, ?, ?, ?)
`, id, owner.TopOrganizationID, owner.OrganizationID, title, nullableJSON(ctxJSON))
	if err != nil {
		return Session{}, fmt.Errorf("create session: %w", err)
	}
	return s.GetByID(ctx, owner, id)
}

// GetByID fetches a session by ID filtered by owner and deleted_at IS NULL.
func (s *MySQLSessionStore) GetByID(ctx context.Context, owner Owner, sessionID string) (Session, error) {
	var out Session
	var title sql.NullString
	var ctxRaw sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT id, top_organization_id, organization_id, title, context, message_count, pinned, created_at, updated_at
FROM sessions
WHERE id = ? AND top_organization_id = ? AND organization_id = ? AND deleted_at IS NULL
`, sessionID, owner.TopOrganizationID, owner.OrganizationID).Scan(
		&out.ID,
		&out.TopOrganizationID,
		&out.OrganizationID,
		&title,
		&ctxRaw,
		&out.MessageCount,
		&out.Pinned,
		&out.CreatedAt,
		&out.UpdatedAt,
	)
	if err != nil {
		return Session{}, fmt.Errorf("get session: %w", err)
	}
	if title.Valid {
		out.Title = &title.String
	}
	if ctxRaw.Valid {
		out.Context = json.RawMessage(ctxRaw.String)
	}
	return out, nil
}

// BumpUpdatedAtAndIncCount increments message_count by delta and updates updated_at.
func (s *MySQLSessionStore) BumpUpdatedAtAndIncCount(ctx context.Context, sessionID string, delta int) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE sessions SET message_count = message_count + ?, updated_at = NOW(3)
WHERE id = ? AND deleted_at IS NULL
`, delta, sessionID)
	if err != nil {
		return fmt.Errorf("bump session count: %w", err)
	}
	return nil
}

// nullableJSON returns nil for empty/nil RawMessage, otherwise the string form.
func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}
