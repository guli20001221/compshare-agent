package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// messagesITTableDDL is the messages half. The sessions half deliberately REUSES
// sessionsITTableDDL from sessions_integration_test.go rather than declaring its own: both DDLs
// are CREATE TABLE IF NOT EXISTS, so a second, slightly different definition of `sessions` would
// simply be skipped by whichever test ran second — leaving it with a table missing the columns it
// needs. (This is not hypothetical; the first draft of this file did exactly that and broke
// TestSessionStore_Integration.)
const messagesITTableDDL = `
CREATE TABLE IF NOT EXISTS messages (
  id            CHAR(36)     NOT NULL PRIMARY KEY,
  session_id    CHAR(36)     NOT NULL,
  request_uuid  VARCHAR(64),
  role          VARCHAR(16)  NOT NULL,
  content       TEXT,
  status        VARCHAR(16)  NOT NULL,
  error_code    VARCHAR(64),
  model         VARCHAR(64),
  input_tokens  INT,
  output_tokens INT,
  ttft_ms       INT,
  latency_ms    INT,
  created_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
)`

// A turn's ANSWER and its STATE must belong to the same session.
//
// CommitTurn matches the state half by (sessionID, owner) and used to match the message half by
// (msgID, owner) ALONE. Two sessions of the same owner therefore shared a keyspace: a message from
// session B, committed with session A's state, would satisfy both halves of the transaction. The
// atomicity the type exists to provide would have been guaranteeing the wrong pair — which is
// worse than no atomicity, because it is invisible.
//
// This is a real-database test, and it is the only kind that can prove it: the bug lives in a SQL
// WHERE clause, and no in-memory double can express a WHERE clause it does not have.
//
// ⚠️ It is SKIPPED without COMPSHARE_TEST_MYSQL_DSN, which means CI does not run it today. Said
// plainly: the session-binding fix has an executable gate, but that gate is not currently executed
// on every push. Run it with:
//
//	COMPSHARE_TEST_MYSQL_DSN='postgres://user:pass@127.0.0.1:5432/throwaway?sslmode=disable' \
//	  go test ./internal/store -run TestPostgresTurnStore_MessageIsBoundToTheSession -count=1 -v
func TestPostgresTurnStore_MessageIsBoundToTheSession(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	_, err := db.ExecContext(ctx, sessionsITTableDDL)
	require.NoError(t, err, "create sessions table")
	_, err = db.ExecContext(ctx, messagesITTableDDL)
	require.NoError(t, err, "create messages table")

	// CHAR(36) ids, as in the real schema — a short literal would be blank-padded and stop
	// matching itself.
	const sessA = "aaaaaaaa-0000-0000-0000-00000000000a"
	const sessB = "bbbbbbbb-0000-0000-0000-00000000000b"
	const msgB = "cccccccc-0000-0000-0000-00000000000c"

	owner := Owner{TopOrganizationID: 4294967100, OrganizationID: 4294967101}
	cleanup := func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM messages WHERE session_id IN ($1,$2)", sessA, sessB)
		_, _ = db.ExecContext(ctx, "DELETE FROM sessions WHERE id IN ($1,$2)", sessA, sessB)
	}
	cleanup()
	t.Cleanup(cleanup)

	for _, sid := range []string{sessA, sessB} {
		_, err := db.ExecContext(ctx,
			`INSERT INTO sessions (id, top_organization_id, organization_id, context_version) VALUES ($1,$2,$3,0)`,
			sid, owner.TopOrganizationID, owner.OrganizationID)
		require.NoError(t, err)
	}
	// The assistant row belongs to session B.
	_, err = db.ExecContext(ctx,
		`INSERT INTO messages (id, session_id, role, content, status) VALUES ($1,$2,'assistant','','pending')`,
		msgB, sessB)
	require.NoError(t, err)

	ts := NewTurnStore(db)

	// Commit session A's STATE together with session B's MESSAGE. The two halves do not belong
	// to the same conversation and the transaction must refuse the pair outright.
	_, err = ts.CommitTurn(ctx, owner, sessA, msgB,
		AssistantPatch{Content: "答案属于 B，状态属于 A", Status: "ok"},
		json.RawMessage(`{"agent_session_state":{"schema_version":"4.0"}}`), 0)

	require.Error(t, err,
		"a message from another session must not be committable with this session's state")

	// And nothing may have landed — not the message, not the state.
	var content, status string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT content, status FROM messages WHERE id = $1`, msgB).Scan(&content, &status))
	assert.Empty(t, content, "session B's row must be untouched")
	assert.Equal(t, "pending", status)

	var version int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT context_version FROM sessions WHERE id = $1`, sessA).Scan(&version))
	assert.Equal(t, 0, version, "session A's state must not have advanced on a rejected pair")
}
