package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// MySQLMessageStore implements MessageStore using MySQL.
type MySQLMessageStore struct {
	db *sql.DB
}

// NewMessageStore returns a new MySQLMessageStore.
func NewMessageStore(db *sql.DB) *MySQLMessageStore {
	return &MySQLMessageStore{db: db}
}

// Append inserts a new message row.
func (s *MySQLMessageStore) Append(ctx context.Context, m Message) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO messages (id, session_id, request_uuid, role, content, status, error_code, model, input_tokens, output_tokens, ttft_ms, latency_ms, metadata)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, m.ID, m.SessionID, nullableRequestUUID(m.RequestUUID), m.Role, m.Content, m.Status,
		nullableStringPtr(m.ErrorCode), nullableStringPtr(m.Model),
		nullableIntPtr(m.InputTokens), nullableIntPtr(m.OutputTokens),
		nullableIntPtr(m.TTFTMs), nullableIntPtr(m.LatencyMs),
		nullableJSON(m.Metadata))
	if err != nil {
		return fmt.Errorf("append message: %w", err)
	}
	return nil
}

// UpdateAssistant patches an assistant message row after LLM response.
// It JOINs sessions to enforce owner scoping. Returns sql.ErrNoRows if no row
// was matched (message absent, wrong owner, or already deleted session).
func (s *MySQLMessageStore) UpdateAssistant(ctx context.Context, owner Owner, msgID string, patch AssistantPatch) error {
	res, err := s.db.ExecContext(ctx, `
UPDATE messages m
JOIN sessions s ON s.id = m.session_id
SET m.content = ?, m.status = ?, m.error_code = ?, m.input_tokens = ?, m.output_tokens = ?, m.ttft_ms = ?, m.latency_ms = ?
WHERE m.id = ? AND m.role = 'assistant'
  AND s.top_organization_id = ? AND s.organization_id = ? AND s.deleted_at IS NULL
`, patch.Content, patch.Status,
		nullableStringPtr(patch.ErrorCode),
		nullableIntPtr(patch.InputTokens), nullableIntPtr(patch.OutputTokens),
		nullableIntPtr(patch.TTFTMs), nullableIntPtr(patch.LatencyMs),
		msgID, owner.TopOrganizationID, owner.OrganizationID)
	if err != nil {
		return fmt.Errorf("update assistant message: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update assistant message rows affected: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListBySession returns messages for a session with cursor-based pagination.
// Limit defaults to 50. Returns (messages, nextCursor, error).
func (s *MySQLMessageStore) ListBySession(ctx context.Context, sessionID string, limit int, cursor string) ([]Message, string, error) {
	if limit <= 0 {
		limit = 50
	}
	queryLimit := limit + 1

	var rows *sql.Rows
	var err error
	if cursor == "" {
		rows, err = s.db.QueryContext(ctx, `
SELECT id, session_id, request_uuid, role, content, status, error_code, model, input_tokens, output_tokens, ttft_ms, latency_ms, metadata, created_at
FROM messages
WHERE session_id = ?
ORDER BY created_at ASC, id ASC
LIMIT ?
`, sessionID, queryLimit)
	} else {
		ts, id, decodeErr := DecodeCursor(cursor)
		if decodeErr != nil {
			return nil, "", fmt.Errorf("list messages: %w", decodeErr)
		}
		rows, err = s.db.QueryContext(ctx, `
SELECT id, session_id, request_uuid, role, content, status, error_code, model, input_tokens, output_tokens, ttft_ms, latency_ms, metadata, created_at
FROM messages
WHERE session_id = ? AND (created_at > ? OR (created_at = ? AND id > ?))
ORDER BY created_at ASC, id ASC
LIMIT ?
`, sessionID, ts, ts, id, queryLimit)
	}
	if err != nil {
		return nil, "", fmt.Errorf("list messages query: %w", err)
	}
	defer rows.Close()

	messages, err := scanMessages(rows)
	if err != nil {
		return nil, "", fmt.Errorf("scan messages: %w", err)
	}

	nextCursor := ""
	if len(messages) > limit {
		last := messages[limit-1]
		nextCursor, err = EncodeCursor(last.CreatedAt, last.ID)
		if err != nil {
			return nil, "", fmt.Errorf("encode next cursor: %w", err)
		}
		messages = messages[:limit]
	}
	return messages, nextCursor, nil
}

// GetWithOwnerCheck fetches a message by ID and verifies the owner via a JOIN
// through sessions. Returns sql.ErrNoRows when not found or unauthorized.
func (s *MySQLMessageStore) GetWithOwnerCheck(ctx context.Context, owner Owner, msgID string) (Message, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT m.id, m.session_id, m.request_uuid, m.role, m.content, m.status, m.error_code, m.model, m.input_tokens, m.output_tokens, m.ttft_ms, m.latency_ms, m.metadata, m.created_at
FROM messages m
JOIN sessions sess ON sess.id = m.session_id
WHERE m.id = ? AND sess.top_organization_id = ? AND sess.organization_id = ? AND sess.deleted_at IS NULL
`, msgID, owner.TopOrganizationID, owner.OrganizationID)
	if err != nil {
		return Message{}, fmt.Errorf("get message with owner check: %w", err)
	}
	defer rows.Close()

	messages, err := scanMessages(rows)
	if err != nil {
		return Message{}, fmt.Errorf("scan message: %w", err)
	}
	if len(messages) == 0 {
		return Message{}, sql.ErrNoRows
	}
	return messages[0], nil
}

func scanMessages(rows *sql.Rows) ([]Message, error) {
	var messages []Message
	for rows.Next() {
		var m Message
		var requestUUID, errorCode, model, metadata sql.NullString
		var inputTokens, outputTokens, ttftMs, latencyMs sql.NullInt64
		if err := rows.Scan(
			&m.ID, &m.SessionID, &requestUUID, &m.Role, &m.Content, &m.Status,
			&errorCode, &model,
			&inputTokens, &outputTokens, &ttftMs, &latencyMs,
			&metadata, &m.CreatedAt,
		); err != nil {
			return nil, err
		}
		if requestUUID.Valid {
			m.RequestUUID = &requestUUID.String
		}
		if errorCode.Valid {
			m.ErrorCode = &errorCode.String
		}
		if model.Valid {
			m.Model = &model.String
		}
		if inputTokens.Valid {
			v := int(inputTokens.Int64)
			m.InputTokens = &v
		}
		if outputTokens.Valid {
			v := int(outputTokens.Int64)
			m.OutputTokens = &v
		}
		if ttftMs.Valid {
			v := int(ttftMs.Int64)
			m.TTFTMs = &v
		}
		if latencyMs.Valid {
			v := int(latencyMs.Int64)
			m.LatencyMs = &v
		}
		if metadata.Valid {
			m.Metadata = json.RawMessage(metadata.String)
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

// nullableRequestUUID returns nil for nil pointer or empty string, otherwise the string value.
func nullableRequestUUID(v *string) any {
	if v == nil || *v == "" {
		return nil
	}
	return *v
}

func nullableStringPtr(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullableIntPtr(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}
