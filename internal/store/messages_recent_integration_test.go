package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestListRecentBySessionReturnsNewestRowsChronologically(t *testing.T) {
	db := openIsolatedMessageTestDB(t)
	ctx := context.Background()
	sessionID := uuid.NewString()
	otherSessionID := uuid.NewString()
	for _, id := range []string{sessionID, otherSessionID} {
		_, err := db.ExecContext(ctx, `INSERT INTO sessions (id, top_organization_id, organization_id) VALUES ($1, 1, 2)`, id)
		require.NoError(t, err)
	}
	base := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 143; i++ {
		role, status := "user", "ok"
		if i%2 == 1 {
			role = "assistant"
		}
		if i == 141 {
			status = "aborted"
		}
		_, err := db.ExecContext(ctx, `
INSERT INTO messages (id, session_id, role, content, status, metadata, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			fmt.Sprintf("00000000-0000-0000-0000-%012d", i), sessionID, role,
			fmt.Sprintf("message-%03d", i), status, `{"marker":"retained"}`,
			base.Add(time.Duration(i/2)*time.Second)) // Pairs share a timestamp: ID breaks ties.
		require.NoError(t, err)
	}
	_, err := db.ExecContext(ctx, `
INSERT INTO messages (id, session_id, role, content, status, created_at)
VALUES ($1, $2, 'user', 'unrelated newer session', 'ok', $3)`, uuid.NewString(), otherSessionID, base.Add(time.Hour))
	require.NoError(t, err)

	messageStore := NewMessageStore(db)
	rows, err := messageStore.ListRecentBySession(ctx, sessionID, 100)
	require.NoError(t, err)
	require.Len(t, rows, 100)
	for i, row := range rows {
		require.Equal(t, fmt.Sprintf("message-%03d", i+43), row.Content)
		require.Equal(t, sessionID, row.SessionID)
		require.JSONEq(t, `{"marker":"retained"}`, string(row.Metadata))
	}
	require.Equal(t, "aborted", rows[98].Status)
	require.Equal(t, "user", rows[99].Role, "the latest unanswered user row is included")

	// UI pagination is a separate contract and must still start at the oldest row.
	page, cursor, err := messageStore.ListBySession(ctx, sessionID, 2, "")
	require.NoError(t, err)
	require.Len(t, page, 2)
	require.Equal(t, "message-000", page[0].Content)
	require.Equal(t, "message-001", page[1].Content)
	require.NotEmpty(t, cursor)
	empty, err := messageStore.ListRecentBySession(ctx, uuid.NewString(), 100)
	require.NoError(t, err)
	require.Empty(t, empty)
}
