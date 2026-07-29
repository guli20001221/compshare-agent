package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestMigrationsApplyTwiceCleanly is what lets the deploy README be one command.
//
// The README tells an operator to apply `deploy/migrations/*.sql` in order,
// without knowing which of them the target database already has. That is only
// honest if re-applying an applied file is a no-op. It was not: the production
// deployment (GitLab 127f4e8a) ships migrations 0001-0004, everything from 0005
// on was bare `CREATE TABLE` / `ADD COLUMN` / `CREATE TRIGGER`, and a blind
// re-run died on the first file under ON_ERROR_STOP=1 — so the instruction had
// to be replaced by a paragraph telling the operator to work out the delta.
//
// Applying repeatedly into a scratch schema is the whole contract. A new
// migration that forgets IF NOT EXISTS (or a CREATE TRIGGER without its DROP)
// fails here, on the second pass, before the README's promise quietly breaks.
func TestMigrationsApplyTwiceCleanly(t *testing.T) {
	db := openTestDB(t)
	dir := filepath.Join("..", "..", "deploy", "migrations")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)
	require.NotEmpty(t, files, "no migrations found; the test would pass vacuously")

	// A scratch schema, not a scratch database: CREATE DATABASE needs a
	// connection to another database, and the migrations use unqualified names,
	// so a private search_path is enough to keep this off the shared tables.
	const schema = "mig_idempotency_check"
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	drop := func() {
		_, _ = db.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	}
	drop()
	t.Cleanup(drop)
	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)

	apply := func(pass int) {
		for _, name := range files {
			body, err := os.ReadFile(filepath.Join(dir, name))
			require.NoError(t, err)
			// search_path is set inside every batch: lib/pq may hand each Exec to
			// a different pooled connection, and the setting is per-session.
			_, err = db.ExecContext(ctx, "SET search_path TO "+schema+";\n"+string(body))
			require.NoError(t, err, "pass %d: %s", pass, name)
		}
	}

	apply(1)
	first := schemaFingerprint(ctx, t, db, schema)
	require.NotEmpty(t, first, "the migrations created nothing in the scratch schema")

	apply(2)
	require.Equal(t, first, schemaFingerprint(ctx, t, db, schema),
		"re-applying every migration changed the schema — clean exit codes are not "+
			"the contract; the second run must leave the database exactly as the first did")
}

// schemaFingerprint reads back the columns, indexes and constraints the scratch
// schema ended up with, as one comparable string. Exit codes alone would let a
// migration that drops and recreates something with different columns pass.
func schemaFingerprint(ctx context.Context, t *testing.T, db *sql.DB, schema string) string {
	t.Helper()
	const q = `
SELECT 'COL ' || table_name || '.' || column_name || ':' || data_type
  FROM information_schema.columns WHERE table_schema = $1
UNION ALL
SELECT 'IDX ' || indexname || ' ' || indexdef FROM pg_indexes WHERE schemaname = $1
UNION ALL
SELECT 'CON ' || conname || ' ' || pg_get_constraintdef(c.oid)
  FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace WHERE n.nspname = $1
ORDER BY 1`
	rows, err := db.QueryContext(ctx, q, schema)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		out = append(out, line)
	}
	require.NoError(t, rows.Err())
	return strings.Join(out, "\n")
}
