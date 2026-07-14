package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessageStore_ListCommittedTail_MergesStrictLegacyAndV2History(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 82001, OrganizationID: 82002}
	otherOwner := Owner{TopOrganizationID: 82001, OrganizationID: 82999}
	session, err := NewSessionStore(db).Create(ctx, owner, nil, nil)
	require.NoError(t, err)
	otherSession, err := NewSessionStore(db).Create(ctx, owner, nil, nil)
	require.NoError(t, err)

	base := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	for seq := 1; seq <= 5; seq++ {
		insertLegacyTailPair(t, db, session.ID, fmt.Sprintf("legacy-%d", seq), seq,
			"ok", "ok", base.Add(time.Duration(seq)*time.Minute))
	}

	// The request UUID is only unique inside a session. Rows in another session
	// must neither join this history nor make a valid local pair look duplicated.
	insertLegacyTailPair(t, db, otherSession.ID, "legacy-5", 50,
		"ok", "ok", base.Add(50*time.Minute))

	// Newer malformed legacy shapes must all be ignored. They exercise each
	// condition that historically produced a half or misleading prompt turn.
	insertLegacyMessage(t, db, session.ID, "legacy-half", "user", "ok", "half-user", base.Add(6*time.Minute))
	insertLegacyTailPair(t, db, session.ID, "legacy-error", 7,
		"ok", "error", base.Add(7*time.Minute))
	insertLegacyTailPair(t, db, session.ID, "legacy-pending", 8,
		"pending", "pending", base.Add(8*time.Minute))
	insertLegacyTailPair(t, db, session.ID, "legacy-aborted", 81,
		"aborted", "aborted", base.Add(8*time.Minute+30*time.Second))
	insertLegacyTailPair(t, db, session.ID, "legacy-duplicate", 9,
		"ok", "ok", base.Add(9*time.Minute))
	insertLegacyMessage(t, db, session.ID, "legacy-duplicate", "user", "ok", "duplicate-user", base.Add(9*time.Minute))
	insertLegacyMessage(t, db, session.ID, "", "user", "ok", "null-user", base.Add(10*time.Minute))
	insertLegacyMessage(t, db, session.ID, "", "assistant", "ok", "null-assistant", base.Add(10*time.Minute))
	insertLegacyTailPair(t, db, session.ID, "legacy-empty", 10,
		"ok", "ok", base.Add(10*time.Minute+30*time.Second))
	_, err = db.Exec(`UPDATE messages SET content = E' \t\n' WHERE session_id = $1 AND request_uuid = 'legacy-empty' AND role = 'user'`, session.ID)
	require.NoError(t, err)

	for seq := 1; seq <= 3; seq++ {
		insertTailFixtureTurnAt(t, db, owner, session.ID, seq, TurnStatusCommitted,
			"ok", "ok", true, base.Add(time.Duration(10+seq)*time.Minute))
	}
	insertTailFixtureTurnAt(t, db, owner, session.ID, 4, TurnStatusCommitted,
		"ok", "ok", true, base.Add(14*time.Minute))
	_, err = db.Exec(`
UPDATE messages
SET content = E'\t\n '
WHERE id = (
  SELECT assistant_message_id FROM chat_turns
  WHERE session_id = $1 AND client_turn_id = 'fixture-004'
)
	`, session.ID)
	require.NoError(t, err)
	// There is deliberately no FK here: the turn points at a foreign-session
	// assistant while a plausible local impostor carries its turn_id.
	insertMisattachedV2Turn(t, db, owner, session.ID, otherSession.ID, 5, base.Add(15*time.Minute))

	got, err := NewMessageStore(db).ListCommittedTail(ctx, owner, session.ID, 6)
	require.NoError(t, err)
	require.Len(t, got, 12)
	assert.Equal(t, []string{
		"legacy-u-3", "legacy-a-3",
		"legacy-u-4", "legacy-a-4",
		"legacy-u-5", "legacy-a-5",
		"u-001", "a-001",
		"u-002", "a-002",
		"u-003", "a-003",
	}, messageContents(got))
	for i, msg := range got {
		if i%2 == 0 {
			assert.Equal(t, "user", msg.Role)
		} else {
			assert.Equal(t, "assistant", msg.Role)
		}
		assert.Equal(t, "ok", msg.Status)
	}

	foreign, err := NewMessageStore(db).ListCommittedTail(ctx, otherOwner, session.ID, 6)
	require.NoError(t, err)
	assert.Empty(t, foreign, "legacy history must remain owner-scoped")
}

func TestMessageStore_ListCommittedTail_V2SequenceBeatsReversedTimestamps(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 83001, OrganizationID: 83002}
	session, err := NewSessionStore(db).Create(ctx, owner, nil, nil)
	require.NoError(t, err)

	base := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	// turn_seq is allocated under the conversation lock and is authoritative.
	// Timestamps can still run backwards when a transaction begins and is then
	// descheduled before it reaches that lock.
	insertTailFixtureTurnAt(t, db, owner, session.ID, 1, TurnStatusCommitted,
		"ok", "ok", true, base.Add(2*time.Minute))
	insertTailFixtureTurnAt(t, db, owner, session.ID, 2, TurnStatusCommitted,
		"ok", "ok", true, base.Add(1*time.Minute))
	insertTailFixtureTurnAt(t, db, owner, session.ID, 3, TurnStatusCommitted,
		"ok", "ok", true, base.Add(3*time.Minute))

	got, err := NewMessageStore(db).ListCommittedTail(ctx, owner, session.ID, 3)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"u-001", "a-001",
		"u-002", "a-002",
		"u-003", "a-003",
	}, messageContents(got))
}

func TestMessageStore_ListCommittedTail_V2LimitUsesSequenceNotTimestamp(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 84001, OrganizationID: 84002}
	session, err := NewSessionStore(db).Create(ctx, owner, nil, nil)
	require.NoError(t, err)

	base := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	for seq := 1; seq <= 60; seq++ {
		insertTailFixtureTurnAt(t, db, owner, session.ID, seq, TurnStatusCommitted,
			"ok", "ok", true, base.Add(time.Duration(seq)*time.Minute))
	}
	// The newest protocol turn has the oldest timestamp. A timestamp tail would
	// discard turn 61 and keep turn 1, which is exactly the context-loss bug.
	insertTailFixtureTurnAt(t, db, owner, session.ID, 61, TurnStatusCommitted,
		"ok", "ok", true, base.Add(-time.Hour))

	got, err := NewMessageStore(db).ListCommittedTail(ctx, owner, session.ID, 60)
	require.NoError(t, err)
	require.Len(t, got, 120)
	assert.Equal(t, "u-002", got[0].Content)
	assert.Equal(t, "a-002", got[1].Content)
	assert.Equal(t, "u-061", got[118].Content)
	assert.Equal(t, "a-061", got[119].Content)
}

func TestMessageStore_ListCommittedTail_StableMergeSupportsRolloutAndRollback(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 85001, OrganizationID: 85002}
	session, err := NewSessionStore(db).Create(ctx, owner, nil, nil)
	require.NoError(t, err)

	base := time.Date(2026, 7, 14, 11, 0, 0, 0, time.UTC)
	// Legacy before rollout.
	insertLegacyTailPair(t, db, session.ID, "legacy-before", 1,
		"ok", "ok", base.Add(1*time.Minute))
	// First v2 deployment.
	insertTailFixtureTurnAt(t, db, owner, session.ID, 1, TurnStatusCommitted,
		"ok", "ok", true, base.Add(2*time.Minute))
	insertTailFixtureTurnAt(t, db, owner, session.ID, 2, TurnStatusCommitted,
		"ok", "ok", true, base.Add(4*time.Minute))
	// A rollback writes legacy history between two v2 commit times.
	insertLegacyTailPair(t, db, session.ID, "legacy-rollback", 2,
		"ok", "ok", base.Add(3*time.Minute))
	// Re-rollout continues the v2 sequence, even if its timestamp is behind the
	// preceding v2 turn. Source order wins; timestamps only merge the two sources.
	insertTailFixtureTurnAt(t, db, owner, session.ID, 3, TurnStatusCommitted,
		"ok", "ok", true, base.Add(3*time.Minute+30*time.Second))
	insertLegacyTailPair(t, db, session.ID, "legacy-after", 3,
		"ok", "ok", base.Add(6*time.Minute))

	got, err := NewMessageStore(db).ListCommittedTail(ctx, owner, session.ID, 6)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"legacy-u-1", "legacy-a-1",
		"u-001", "a-001",
		"legacy-u-2", "legacy-a-2",
		"u-002", "a-002",
		"u-003", "a-003",
		"legacy-u-3", "legacy-a-3",
	}, messageContents(got))
}

func insertMisattachedV2Turn(
	t *testing.T,
	db *sql.DB,
	owner Owner,
	sessionID, otherSessionID string,
	seq int,
	createdAt time.Time,
) {
	t.Helper()
	turnID := uuid.NewString()
	userID := uuid.NewString()
	foreignAssistantID := uuid.NewString()
	localImpostorAssistantID := uuid.NewString()
	_, err := db.Exec(`
INSERT INTO chat_turns
  (id, session_id, top_organization_id, organization_id, client_turn_id,
   turn_seq, request_hash, status, user_message_id, assistant_message_id,
   base_context_version, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'committed', $8, $9, 0, $10, $10)
`, turnID, sessionID, owner.TopOrganizationID, owner.OrganizationID,
		fmt.Sprintf("misattached-%d", seq), seq, HashTurnRequest("misattached"),
		userID, foreignAssistantID, createdAt)
	require.NoError(t, err)
	_, err = db.Exec(`
INSERT INTO messages (id, session_id, role, content, status, turn_id, turn_role, created_at)
VALUES
  ($1, $2, 'user', 'misattached-user', 'ok', $4, 'user', $5),
  ($3, $2, 'assistant', 'local-impostor', 'ok', $4, 'assistant', $5),
  ($6, $7, 'assistant', 'foreign-expected-assistant', 'ok', NULL, NULL, $5)
`, userID, sessionID, localImpostorAssistantID, turnID, createdAt,
		foreignAssistantID, otherSessionID)
	require.NoError(t, err)
}

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
	insertTailFixtureTurnAt(t, db, owner, sessionID, seq, turnStatus,
		userStatus, assistantStatus, includeAssistant, time.Now().UTC().Add(time.Duration(seq)*time.Millisecond))
}

func insertTailFixtureTurnAt(
	t *testing.T,
	db *sql.DB,
	owner Owner,
	sessionID string,
	seq int,
	turnStatus TurnStatus,
	userStatus, assistantStatus string,
	includeAssistant bool,
	createdAt time.Time,
) {
	t.Helper()
	turnID := uuid.NewString()
	userID := uuid.NewString()
	assistantID := uuid.NewString()
	_, err := db.Exec(`
INSERT INTO chat_turns
  (id, session_id, top_organization_id, organization_id, client_turn_id,
   turn_seq, request_hash, status, user_message_id, assistant_message_id,
   base_context_version, next_retry_at, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8::varchar, $9, $10, 0,
		CASE WHEN $8::varchar = 'failed_retryable' THEN $11::timestamptz ELSE NULL END, $11, $11)
`, turnID, sessionID, owner.TopOrganizationID, owner.OrganizationID,
		fmt.Sprintf("fixture-%03d", seq), seq, HashTurnRequest(fmt.Sprintf("turn-%03d", seq)),
		turnStatus, userID, assistantID, createdAt)
	require.NoError(t, err)
	_, err = db.Exec(`
INSERT INTO messages (id, session_id, role, content, status, turn_id, turn_role, created_at)
VALUES ($1, $2, 'user', $3, $4, $5, 'user', $6)
`, userID, sessionID, fmt.Sprintf("u-%03d", seq), userStatus, turnID, createdAt)
	require.NoError(t, err)
	if !includeAssistant {
		return
	}
	_, err = db.Exec(`
INSERT INTO messages (id, session_id, role, content, status, turn_id, turn_role, created_at)
VALUES ($1, $2, 'assistant', $3, $4, $5, 'assistant', $6)
`, assistantID, sessionID, fmt.Sprintf("a-%03d", seq), assistantStatus, turnID, createdAt.Add(time.Second))
	require.NoError(t, err)
}

func insertLegacyTailPair(
	t *testing.T,
	db *sql.DB,
	sessionID string,
	requestUUID string,
	seq int,
	userStatus, assistantStatus string,
	createdAt time.Time,
) {
	t.Helper()
	insertLegacyMessage(t, db, sessionID, requestUUID, "user", userStatus,
		fmt.Sprintf("legacy-u-%d", seq), createdAt)
	insertLegacyMessage(t, db, sessionID, requestUUID, "assistant", assistantStatus,
		fmt.Sprintf("legacy-a-%d", seq), createdAt.Add(time.Second))
}

func insertLegacyMessage(
	t *testing.T,
	db *sql.DB,
	sessionID, requestUUID, role, status, content string,
	createdAt time.Time,
) {
	t.Helper()
	var request any = requestUUID
	if requestUUID == "" {
		request = nil
	}
	_, err := db.Exec(`
INSERT INTO messages
  (id, session_id, request_uuid, role, content, status, turn_id, turn_role, created_at)
VALUES ($1, $2, $3, $4, $5, $6, NULL, NULL, $7)
`, uuid.NewString(), sessionID, request, role, content, status, createdAt)
	require.NoError(t, err)
}

func messageContents(messages []Message) []string {
	contents := make([]string, 0, len(messages))
	for _, msg := range messages {
		contents = append(contents, msg.Content)
	}
	return contents
}
