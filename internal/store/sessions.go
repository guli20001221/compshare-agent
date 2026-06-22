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
VALUES ($1, $2, $3, $4, $5)
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
SELECT id, top_organization_id, organization_id, title, context, context_version, message_count, pinned, created_at, updated_at
FROM sessions
WHERE id = $1 AND top_organization_id = $2 AND organization_id = $3 AND deleted_at IS NULL
`, sessionID, owner.TopOrganizationID, owner.OrganizationID).Scan(
		&out.ID,
		&out.TopOrganizationID,
		&out.OrganizationID,
		&title,
		&ctxRaw,
		&out.ContextVersion,
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
// Owner scoping is enforced via top_organization_id + organization_id + deleted_at IS NULL.
// Returns sql.ErrNoRows when the session does not exist or does not belong to owner.
func (s *MySQLSessionStore) BumpUpdatedAtAndIncCount(ctx context.Context, owner Owner, sessionID string, delta int) error {
	res, err := s.db.ExecContext(ctx, `
UPDATE sessions SET message_count = message_count + $1, updated_at = NOW()
WHERE id = $2 AND top_organization_id = $3 AND organization_id = $4 AND deleted_at IS NULL
`, delta, sessionID, owner.TopOrganizationID, owner.OrganizationID)
	if err != nil {
		return fmt.Errorf("bump session count: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("bump session count rows affected: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UpdateContext atomically writes ctxJSON into sessions.context and
// increments sessions.context_version with optimistic concurrency. The
// WHERE clause checks owner, deleted_at IS NULL, AND context_version =
// expectedVersion — any mismatch yields 0 rows affected and is reported
// as ErrStaleWrite. The new context_version (expectedVersion + 1) is
// computed on success without RETURNING since MySQL 8.0 lacks it for UPDATE.
func (s *MySQLSessionStore) UpdateContext(
	ctx context.Context, owner Owner, sessionID string,
	ctxJSON json.RawMessage, expectedVersion int,
) (int, error) {
	res, err := s.db.ExecContext(ctx, `
UPDATE sessions
   SET context = $1, context_version = context_version + 1
 WHERE id = $2
   AND top_organization_id = $3
   AND organization_id = $4
   AND deleted_at IS NULL
   AND context_version = $5
`,
		nullableJSON(ctxJSON),
		sessionID,
		owner.TopOrganizationID,
		owner.OrganizationID,
		expectedVersion,
	)
	if err != nil {
		return 0, fmt.Errorf("update session context: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("update session context rows affected: %w", err)
	}
	if n == 0 {
		return 0, ErrStaleWrite
	}
	return expectedVersion + 1, nil
}

// ListByOwner returns up to limit of the owner's sessions, most recently
// active first. Soft-deleted rows are excluded. Uses idx_owner_updated
// (top_organization_id, organization_id, updated_at) via reverse index scan.
// The SELECT column list mirrors GetByID for scan parity; context is read but
// the history sidebar does not use it.
func (s *MySQLSessionStore) ListByOwner(ctx context.Context, owner Owner, limit int) ([]Session, error) {
	if limit < 1 {
		// The HTTP handler clamps to [1,50]; this guards the exported method
		// against a future caller passing <=0, which would otherwise panic at
		// make(cap) below or emit LIMIT 0 / a negative LIMIT to the engine.
		return []Session{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, top_organization_id, organization_id, title, context, context_version, message_count, pinned, created_at, updated_at
FROM sessions
WHERE top_organization_id = $1 AND organization_id = $2 AND deleted_at IS NULL
ORDER BY updated_at DESC
LIMIT $3
`, owner.TopOrganizationID, owner.OrganizationID, limit)
	if err != nil {
		return nil, fmt.Errorf("list sessions by owner: %w", err)
	}
	defer rows.Close()

	out := make([]Session, 0, limit)
	for rows.Next() {
		var sess Session
		var title sql.NullString
		var ctxRaw sql.NullString
		if err := rows.Scan(
			&sess.ID,
			&sess.TopOrganizationID,
			&sess.OrganizationID,
			&title,
			&ctxRaw,
			&sess.ContextVersion,
			&sess.MessageCount,
			&sess.Pinned,
			&sess.CreatedAt,
			&sess.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan session row: %w", err)
		}
		if title.Valid {
			sess.Title = &title.String
		}
		if ctxRaw.Valid {
			sess.Context = json.RawMessage(ctxRaw.String)
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session rows: %w", err)
	}
	return out, nil
}

// SetTitleIfEmpty sets title only when it is currently NULL, preserving an
// explicit client-set title. Owner-scoped + deleted_at IS NULL. 0 rows affected
// (title already present, or row missing) is intentionally NOT an error: this is
// a best-effort first-turn derivation, never fatal to the chat turn.
func (s *MySQLSessionStore) SetTitleIfEmpty(ctx context.Context, owner Owner, sessionID string, title string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE sessions SET title = $1
WHERE id = $2 AND top_organization_id = $3 AND organization_id = $4 AND deleted_at IS NULL AND title IS NULL
`, title, sessionID, owner.TopOrganizationID, owner.OrganizationID)
	if err != nil {
		return fmt.Errorf("set session title if empty: %w", err)
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
