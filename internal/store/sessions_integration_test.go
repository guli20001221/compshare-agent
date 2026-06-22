package store

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sessionsITTableDDL mirrors deploy/migrations/0001_init.sql + 0003 (the
// context_version column) for the sessions table only, so the integration test
// is self-contained. ListByOwner / SetTitleIfEmpty / Create /
// BumpUpdatedAtAndIncCount touch only this table. (PostgreSQL; the prod 0001
// adds an ON-UPDATE trigger for updated_at — omitted here since the assertions
// rely only on BumpUpdatedAtAndIncCount's explicit updated_at = NOW().)
const sessionsITTableDDL = `
CREATE TABLE IF NOT EXISTS sessions (
  id                   CHAR(36)     NOT NULL PRIMARY KEY,
  top_organization_id  BIGINT       NOT NULL,
  organization_id      BIGINT       NOT NULL,
  title                VARCHAR(255),
  context              JSONB,
  context_version      INT          NOT NULL DEFAULT 0,
  message_count        INT          NOT NULL DEFAULT 0,
  pinned               BOOLEAN      NOT NULL DEFAULT FALSE,
  created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
  updated_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
  deleted_at           TIMESTAMPTZ
)`

// openTestDB opens COMPSHARE_TEST_MYSQL_DSN (value is a PostgreSQL DSN; env name
// kept for deploy-template stability). The test is SKIPPED when the var is unset
// (so it never runs in the normal `go test ./...` suite), and it REFUSES to run
// against the production PostgreSQL host so a misconfiguration can never write
// test rows to prod.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("COMPSHARE_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("COMPSHARE_TEST_MYSQL_DSN not set — skipping real-DB integration test")
	}
	if strings.Contains(dsn, "117.50.198.43") {
		t.Fatal("refusing to run the integration test against the production PostgreSQL host")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, db.PingContext(ctx), "ping test DB")
	return db
}

// TestSessionStore_Integration runs the REAL SQL of ListByOwner and
// SetTitleIfEmpty (plus Create + BumpUpdatedAtAndIncCount) against a live
// MySQL/TiDB-compatible server, then reads rows back with a raw SELECT to prove
// the statements parse, execute, and persist. This is the execution coverage the
// in-memory mock cannot provide: a typo in the SELECT column list, a dropped
// organization_id in the WHERE (cross-tenant leak), an ASC/DESC flip, a broken
// deleted_at filter, or a wrong title IS NULL predicate would all fail here.
//
// Run it with:
//
//	COMPSHARE_TEST_MYSQL_DSN='user:pass@tcp(127.0.0.1:3306)/throwaway_db' \
//	  go test ./internal/store -run TestSessionStore_Integration -count=1 -v
func TestSessionStore_Integration(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	_, err := db.ExecContext(ctx, sessionsITTableDDL)
	require.NoError(t, err, "create sessions table")

	// ownerA and ownerB share a top_organization_id but differ on
	// organization_id, so the cross-tenant assertion specifically exercises the
	// organization_id leg of the WHERE clause. Distinctive high IDs keep the
	// test rows clear of any real data and make cleanup precise.
	ownerA := Owner{TopOrganizationID: 4294967200, OrganizationID: 4294967201}
	ownerB := Owner{TopOrganizationID: 4294967200, OrganizationID: 4294967202}

	cleanup := func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM sessions WHERE top_organization_id = $1", ownerA.TopOrganizationID)
	}
	cleanup()
	t.Cleanup(cleanup)

	ss := NewSessionStore(db)

	a1, err := ss.Create(ctx, ownerA, nil, nil)
	require.NoError(t, err, "create a1")
	a2, err := ss.Create(ctx, ownerA, nil, nil)
	require.NoError(t, err, "create a2")
	b1, err := ss.Create(ctx, ownerB, nil, nil)
	require.NoError(t, err, "create b1 (other org)")

	// SetTitleIfEmpty sets when NULL, then is a no-op that preserves the title.
	// NOTE: SetTitleIfEmpty issues an UPDATE, so it fires ON UPDATE
	// CURRENT_TIMESTAMP and bumps a1.updated_at — harmless in production because
	// the chat write path always calls BumpUpdatedAtAndIncCount right after, but
	// it means the title must be set BEFORE we establish the updated_at ordering
	// below, or a1 would jump ahead of a2.
	require.NoError(t, ss.SetTitleIfEmpty(ctx, ownerA, a1.ID, "first title"))
	require.NoError(t, ss.SetTitleIfEmpty(ctx, ownerA, a1.ID, "SHOULD NOT OVERWRITE"))

	// Raw read-back proves the title actually persisted (independent of the store).
	var gotTitle sql.NullString
	require.NoError(t, db.QueryRowContext(ctx, "SELECT title FROM sessions WHERE id = $1", a1.ID).Scan(&gotTitle))
	require.True(t, gotTitle.Valid, "title must be persisted, not NULL")
	assert.Equal(t, "first title", gotTitle.String, "SetTitleIfEmpty must NOT overwrite an existing title")

	// Now establish ordering: bump a1 then a2 (each sets updated_at = NOW(3)) so
	// a2 is the most-recently-active row. The later Bump overwrites the title
	// UPDATE's timestamp side-effect, mirroring the production turn sequence.
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, ss.BumpUpdatedAtAndIncCount(ctx, ownerA, a1.ID, 2))
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, ss.BumpUpdatedAtAndIncCount(ctx, ownerA, a2.ID, 2))

	// limit is honored.
	top1, err := ss.ListByOwner(ctx, ownerA, 1)
	require.NoError(t, err)
	require.Len(t, top1, 1, "LIMIT 1 must return exactly one row")
	assert.Equal(t, a2.ID, top1[0].ID, "most-recently-active row first")
	assert.EqualValues(t, 2, top1[0].MessageCount, "message_count bumped to 2 must persist")

	// ListByOwner: owner-scoped + ordered DESC + carries the persisted title.
	list, err := ss.ListByOwner(ctx, ownerA, 10)
	require.NoError(t, err)
	require.Len(t, list, 2, "ownerA has exactly two non-deleted sessions")
	assert.Equal(t, a2.ID, list[0].ID, "updated_at DESC: a2 first")
	assert.Equal(t, a1.ID, list[1].ID, "updated_at DESC: a1 second")
	for _, s := range list {
		assert.NotEqual(t, b1.ID, s.ID, "ownerB's session must NOT leak into ownerA's list")
	}
	require.NotNil(t, list[1].Title)
	assert.Equal(t, "first title", *list[1].Title, "persisted title surfaces in the list")

	// Soft-deleted rows are excluded.
	_, err = db.ExecContext(ctx, "UPDATE sessions SET deleted_at = now() WHERE id = $1", a2.ID)
	require.NoError(t, err)
	afterDelete, err := ss.ListByOwner(ctx, ownerA, 10)
	require.NoError(t, err)
	require.Len(t, afterDelete, 1, "soft-deleted session must be excluded")
	assert.Equal(t, a1.ID, afterDelete[0].ID)
}
