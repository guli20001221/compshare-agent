package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// UpdateAssistantMetadata's statement is never parsed by a unit test — a
// dialect or semantics error in it is invisible until it reaches a database. It
// runs against a real PostgreSQL schema here for two reasons: to prove the SQL
// executes at all, and to prove it merges rather than assigns. Merging is the
// contract the envelope depends on; assignment would make each writer of
// messages.metadata silently drop every other writer's keys.
func TestUpdateAssistantMetadata_MergesInsteadOfReplacing(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	store := &MySQLMessageStore{db: db}
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 66391350, OrganizationID: 64404856}

	sessionID := uuid.NewString()
	_, err := db.ExecContext(ctx, `
INSERT INTO sessions (id, top_organization_id, organization_id) VALUES ($1, $2, $3)
`, sessionID, owner.TopOrganizationID, owner.OrganizationID)
	require.NoError(t, err)

	assistantID := uuid.NewString()
	_, err = db.ExecContext(ctx, `
INSERT INTO messages (id, session_id, role, content, status, metadata)
VALUES ($1, $2, 'assistant', 'reply', 'ok', $3)
`, assistantID, sessionID, `{"sibling_key":"written by someone else","v":1}`)
	require.NoError(t, err)

	transcript := json.RawMessage(`{"agent_transcript_v1":{"v":1,"messages":[{"role":"tool","content":"{}"}]}}`)
	require.NoError(t, store.UpdateAssistantMetadata(ctx, owner, assistantID, transcript))

	var raw []byte
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT metadata FROM messages WHERE id = $1`, assistantID).Scan(&raw))

	var got map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &got))

	assert.Contains(t, got, "agent_transcript_v1", "the transcript must land")
	assert.Contains(t, got, "sibling_key",
		"a sibling key written by another producer must survive; SET metadata = $1 deletes it")
	assert.JSONEq(t, `"written by someone else"`, string(got["sibling_key"]))
	assert.JSONEq(t, `1`, string(got["v"]), "scalar siblings survive too")
}

// A row with no metadata at all is the ordinary case — the assistant placeholder
// is inserted with a NULL metadata column. `||` against NULL yields NULL in
// PostgreSQL, so without the COALESCE this write would blank the column instead
// of populating it, and the shadow rollout would read as "wrote nothing".
func TestUpdateAssistantMetadata_PopulatesNullMetadata(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	store := &MySQLMessageStore{db: db}
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 66391350, OrganizationID: 64404856}

	sessionID := uuid.NewString()
	_, err := db.ExecContext(ctx, `
INSERT INTO sessions (id, top_organization_id, organization_id) VALUES ($1, $2, $3)
`, sessionID, owner.TopOrganizationID, owner.OrganizationID)
	require.NoError(t, err)

	assistantID := uuid.NewString()
	_, err = db.ExecContext(ctx, `
INSERT INTO messages (id, session_id, role, content, status, metadata)
VALUES ($1, $2, 'assistant', 'reply', 'ok', NULL)
`, assistantID, sessionID)
	require.NoError(t, err)

	require.NoError(t, store.UpdateAssistantMetadata(ctx, owner, assistantID,
		json.RawMessage(`{"agent_transcript_v1":{"v":1}}`)))

	var raw []byte
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT metadata FROM messages WHERE id = $1`, assistantID).Scan(&raw))
	assert.JSONEq(t, `{"agent_transcript_v1":{"v":1}}`, string(raw))
}

// Owner scoping is the same guard UpdateAssistant carries; a shadow write must
// not be a way to stamp a row belonging to another tenant.
func TestUpdateAssistantMetadata_RejectsForeignOwner(t *testing.T) {
	db := openIsolatedTurnTestDB(t)
	store := &MySQLMessageStore{db: db}
	ctx := context.Background()
	owner := Owner{TopOrganizationID: 66391350, OrganizationID: 64404856}

	sessionID := uuid.NewString()
	_, err := db.ExecContext(ctx, `
INSERT INTO sessions (id, top_organization_id, organization_id) VALUES ($1, $2, $3)
`, sessionID, owner.TopOrganizationID, owner.OrganizationID)
	require.NoError(t, err)

	assistantID := uuid.NewString()
	_, err = db.ExecContext(ctx, `
INSERT INTO messages (id, session_id, role, content, status) VALUES ($1, $2, 'assistant', 'reply', 'ok')
`, assistantID, sessionID)
	require.NoError(t, err)

	stranger := Owner{TopOrganizationID: 1, OrganizationID: 2}
	err = store.UpdateAssistantMetadata(ctx, stranger, assistantID, json.RawMessage(`{"agent_transcript_v1":{"v":1}}`))
	assert.ErrorIs(t, err, sql.ErrNoRows, "another tenant must not be able to write this row")
}
