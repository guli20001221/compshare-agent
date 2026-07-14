package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessageStore_ListCommittedTail_RealPostgres(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 81001, OrganizationID: 81002}
	otherOwner := Owner{TopOrganizationID: 81001, OrganizationID: 81999}
	session, err := NewSessionStore(db).Create(ctx, owner, nil, nil)
	require.NoError(t, err)

	// More history than a single engine rebuild keeps. The reader must choose
	// the newest complete committed turns, not the first rows in the session.
	for seq := 1; seq <= 120; seq++ {
		insertTailFixtureTurn(t, db, owner, session.ID, seq, TurnStatusCommitted, "ok", "ok", true)
	}
	// Later non-committed and incomplete turns must not leak a half turn into
	// the model history even though their messages are newer.
	insertTailFixtureTurn(t, db, owner, session.ID, 121, TurnStatusAccepted, "pending", "pending", true)
	insertTailFixtureTurn(t, db, owner, session.ID, 122, TurnStatusFailedRetryable, "error", "error", true)
	insertTailFixtureTurn(t, db, owner, session.ID, 123, TurnStatusCommitted, "ok", "ok", false)

	got, err := NewMessageStore(db).ListCommittedTail(ctx, owner, session.ID, 7)
	require.NoError(t, err)
	require.Len(t, got, 14)
	for i, seq := range []int{114, 115, 116, 117, 118, 119, 120} {
		assert.Equal(t, "user", got[i*2].Role)
		assert.Equal(t, fmt.Sprintf("u-%03d", seq), got[i*2].Content)
		assert.Equal(t, "assistant", got[i*2+1].Role)
		assert.Equal(t, fmt.Sprintf("a-%03d", seq), got[i*2+1].Content)
	}

	foreign, err := NewMessageStore(db).ListCommittedTail(ctx, otherOwner, session.ID, 7)
	require.NoError(t, err)
	assert.Empty(t, foreign, "a caller must not read another owner's conversation")
}

// insertTailFixtureTurn writes exact protocol states without exercising the
// turn-store implementation. That keeps this a focused contract test for the
// history query, including a deliberately incomplete committed turn that the
// normal writer would never create.
func insertTailFixtureTurn(
	t *testing.T,
	db *sql.DB,
	owner Owner,
	sessionID string,
	seq int,
	turnStatus TurnStatus,
	userStatus, assistantStatus string,
	includeAssistant bool,
) {
	t.Helper()
	turnID := uuid.NewString()
	userID := uuid.NewString()
	assistantID := uuid.NewString()
	_, err := db.Exec(`
INSERT INTO chat_turns
  (id, session_id, top_organization_id, organization_id, client_turn_id,
   turn_seq, request_hash, status, user_message_id, assistant_message_id,
   base_context_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 0)
`, turnID, sessionID, owner.TopOrganizationID, owner.OrganizationID,
		fmt.Sprintf("fixture-%03d", seq), seq, HashTurnRequest(fmt.Sprintf("turn-%03d", seq)),
		turnStatus, userID, assistantID)
	require.NoError(t, err)
	_, err = db.Exec(`
INSERT INTO messages (id, session_id, role, content, status, turn_id, turn_role)
VALUES ($1, $2, 'user', $3, $4, $5, 'user')
`, userID, sessionID, fmt.Sprintf("u-%03d", seq), userStatus, turnID)
	require.NoError(t, err)
	if !includeAssistant {
		return
	}
	_, err = db.Exec(`
INSERT INTO messages (id, session_id, role, content, status, turn_id, turn_role)
VALUES ($1, $2, 'assistant', $3, $4, $5, 'assistant')
`, assistantID, sessionID, fmt.Sprintf("a-%03d", seq), assistantStatus, turnID)
	require.NoError(t, err)
}
