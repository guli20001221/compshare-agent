package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
)

// MySQLFeedbackStore implements FeedbackStore using MySQL.
type MySQLFeedbackStore struct {
	db *sql.DB
}

// NewFeedbackStore returns a new MySQLFeedbackStore.
func NewFeedbackStore(db *sql.DB) *MySQLFeedbackStore {
	return &MySQLFeedbackStore{db: db}
}

// Insert stores user feedback for a message. An empty comment is stored as SQL NULL.
// Returns the new feedback row ID.
func (s *MySQLFeedbackStore) Insert(ctx context.Context, msgID, rating, comment string) (string, error) {
	id := uuid.NewString()
	var commentArg any
	if comment != "" {
		commentArg = comment
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO message_feedback (id, message_id, rating, comment)
VALUES (?, ?, ?, ?)
`, id, msgID, rating, commentArg)
	if err != nil {
		return "", fmt.Errorf("insert feedback: %w", err)
	}
	return id, nil
}
