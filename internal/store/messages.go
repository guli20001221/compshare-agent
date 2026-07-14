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
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
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
SET content = $1, status = $2, error_code = $3, input_tokens = $4, output_tokens = $5, ttft_ms = $6, latency_ms = $7
FROM sessions s
WHERE s.id = m.session_id
  AND m.id = $8 AND m.role = 'assistant'
  AND s.top_organization_id = $9 AND s.organization_id = $10 AND s.deleted_at IS NULL
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
WHERE session_id = $1
ORDER BY created_at ASC, id ASC
LIMIT $2
`, sessionID, queryLimit)
	} else {
		ts, id, decodeErr := DecodeCursor(cursor)
		if decodeErr != nil {
			return nil, "", fmt.Errorf("list messages: %w", decodeErr)
		}
		rows, err = s.db.QueryContext(ctx, `
SELECT id, session_id, request_uuid, role, content, status, error_code, model, input_tokens, output_tokens, ttft_ms, latency_ms, metadata, created_at
FROM messages
WHERE session_id = $1 AND (created_at > $2 OR (created_at = $3 AND id > $4))
ORDER BY created_at ASC, id ASC
LIMIT $5
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

// ListCommittedTail returns the newest turnLimit complete, protocol-committed
// user/assistant pairs for one owner-scoped session. Protocol-committed means
// either a committed v2 chat_turn with exactly one ok message per role, or a
// legacy pair with the same non-empty request_uuid and exactly those two ok
// messages. Legacy rows are read in place; this bridge deliberately performs
// no destructive backfill.
func (s *MySQLMessageStore) ListCommittedTail(
	ctx context.Context,
	owner Owner,
	sessionID string,
	turnLimit int,
) ([]Message, error) {
	if turnLimit <= 0 {
		return []Message{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
WITH scoped_session AS (
  SELECT sess.id
  FROM sessions sess
  WHERE sess.id = $1
    AND sess.top_organization_id = $2
    AND sess.organization_id = $3
    AND sess.deleted_at IS NULL
),
v2_pairs AS (
  SELECT
    MAX(m.id) FILTER (
      WHERE m.turn_role = 'user' AND m.role = 'user' AND m.status = 'ok'
        AND m.content ~ '[^[:space:]]'
    ) AS user_message_id,
    MAX(m.id) FILTER (
      WHERE m.turn_role = 'assistant' AND m.role = 'assistant' AND m.status = 'ok'
        AND m.content ~ '[^[:space:]]'
    ) AS assistant_message_id,
    MIN(m.created_at) FILTER (
      WHERE m.turn_role = 'user' AND m.role = 'user' AND m.status = 'ok'
        AND m.content ~ '[^[:space:]]'
    ) AS pair_at,
    'v2:' || t.id AS pair_key
  FROM chat_turns t
  JOIN scoped_session sess ON sess.id = t.session_id
  JOIN messages m
    ON m.turn_id = t.id
   AND m.session_id = t.session_id
   AND (m.id = t.user_message_id OR m.id = t.assistant_message_id)
  WHERE t.top_organization_id = $2
    AND t.organization_id = $3
    AND t.status = 'committed'
  GROUP BY t.id
  HAVING COUNT(*) = 2
     AND COUNT(*) FILTER (
       WHERE m.id = t.user_message_id
         AND m.turn_role = 'user' AND m.role = 'user' AND m.status = 'ok'
         AND m.content ~ '[^[:space:]]'
     ) = 1
     AND COUNT(*) FILTER (
       WHERE m.id = t.assistant_message_id
         AND m.turn_role = 'assistant' AND m.role = 'assistant' AND m.status = 'ok'
         AND m.content ~ '[^[:space:]]'
     ) = 1
),
legacy_pairs AS (
  SELECT
    MAX(m.id) FILTER (
      WHERE m.role = 'user' AND m.status = 'ok' AND m.content ~ '[^[:space:]]'
    ) AS user_message_id,
    MAX(m.id) FILTER (
      WHERE m.role = 'assistant' AND m.status = 'ok' AND m.content ~ '[^[:space:]]'
    ) AS assistant_message_id,
    MIN(m.created_at) FILTER (
      WHERE m.role = 'user' AND m.status = 'ok' AND m.content ~ '[^[:space:]]'
    ) AS pair_at,
    'legacy:' || m.request_uuid AS pair_key
  FROM messages m
  JOIN scoped_session sess ON sess.id = m.session_id
  WHERE m.turn_id IS NULL
    AND m.request_uuid IS NOT NULL
    AND m.request_uuid ~ '[^[:space:]]'
  GROUP BY m.session_id, m.request_uuid
  HAVING COUNT(*) = 2
     AND COUNT(*) FILTER (
       WHERE m.turn_role IS NULL AND m.role = 'user' AND m.status = 'ok'
         AND m.content ~ '[^[:space:]]'
     ) = 1
     AND COUNT(*) FILTER (
       WHERE m.turn_role IS NULL AND m.role = 'assistant' AND m.status = 'ok'
         AND m.content ~ '[^[:space:]]'
     ) = 1
),
eligible_pairs AS (
  SELECT user_message_id, assistant_message_id, pair_at, pair_key FROM v2_pairs
  UNION ALL
  SELECT user_message_id, assistant_message_id, pair_at, pair_key FROM legacy_pairs
),
committed_tail AS (
  SELECT user_message_id, assistant_message_id, pair_at, pair_key
  FROM eligible_pairs
  ORDER BY pair_at DESC, pair_key DESC
  LIMIT $4
)
SELECT m.id, m.session_id, m.request_uuid, m.role, m.content, m.status,
       m.error_code, m.model, m.input_tokens, m.output_tokens, m.ttft_ms,
       m.latency_ms, m.metadata, m.created_at
FROM committed_tail tail
JOIN messages m
  ON m.id = tail.user_message_id OR m.id = tail.assistant_message_id
ORDER BY tail.pair_at ASC, tail.pair_key ASC,
         CASE WHEN m.id = tail.user_message_id THEN 0 ELSE 1 END ASC
`, sessionID, owner.TopOrganizationID, owner.OrganizationID, turnLimit)
	if err != nil {
		return nil, fmt.Errorf("list committed message tail query: %w", err)
	}
	defer rows.Close()

	messages, err := scanMessages(rows)
	if err != nil {
		return nil, fmt.Errorf("scan committed message tail: %w", err)
	}
	return messages, nil
}

// GetWithOwnerCheck fetches a message by ID and verifies the owner via a JOIN
// through sessions. Returns sql.ErrNoRows when not found or unauthorized.
func (s *MySQLMessageStore) GetWithOwnerCheck(ctx context.Context, owner Owner, msgID string) (Message, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT m.id, m.session_id, m.request_uuid, m.role, m.content, m.status, m.error_code, m.model, m.input_tokens, m.output_tokens, m.ttft_ms, m.latency_ms, m.metadata, m.created_at
FROM messages m
JOIN sessions sess ON sess.id = m.session_id
WHERE m.id = $1 AND sess.top_organization_id = $2 AND sess.organization_id = $3 AND sess.deleted_at IS NULL
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
