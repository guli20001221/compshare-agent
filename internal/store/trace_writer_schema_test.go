package store

import (
	"context"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/observability"
	"github.com/stretchr/testify/require"
)

// TestTraceWriterInsertMatchesMigratedSchema runs the real trace INSERT against
// the schema the full migration sequence produces. The column list the writer
// names and the columns the migrations leave behind are two files that only a
// live table can reconcile; a column dropped by a migration but still named by
// the writer fails every batch after deploy while every unit test stays green.
func TestTraceWriterInsertMatchesMigratedSchema(t *testing.T) {
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
	require.NotEmpty(t, files)

	const schema = "trace_writer_schema_check"
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	drop := func() {
		_, _ = db.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	}
	drop()
	t.Cleanup(drop)
	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	for _, name := range files {
		body, readErr := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, readErr)
		_, err = db.ExecContext(ctx, "SET search_path TO "+schema+";\n"+string(body))
		require.NoError(t, err, name)
	}

	// The writer opens its own pool, so the scratch schema travels in the DSN.
	dsn, err := url.Parse(os.Getenv("COMPSHARE_TEST_MYSQL_DSN"))
	require.NoError(t, err)
	query := dsn.Query()
	query.Set("search_path", schema)
	dsn.RawQuery = query.Encode()
	writer, err := observability.NewMySQLWriter(dsn.String(), observability.MySQLWriterOptions{
		BatchSize: 1, FlushPeriod: 50 * time.Millisecond, Logger: log.New(io.Discard, "", 0),
	})
	require.NoError(t, err)

	rec := observability.TraceRecord{TraceID: "trace-writer-schema-check", TurnIndex: 1}
	rec.FinalizeOutcome(observability.FinishSignals{ReactRounds: 1})
	require.NoError(t, writer.Enqueue(observability.TenantContext{TopOrgID: 1, OrgID: 2, ConnectionID: "c"}, rec))
	require.NoError(t, writer.Close(ctx))
	require.Equal(t, uint64(1), writer.Stats().InsertSucceeded, "the INSERT must succeed against the migrated table")

	var n int
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT count(*) FROM "+schema+".agent_traces WHERE request_uuid = $1", rec.TraceID).Scan(&n))
	require.Equal(t, 1, n)
}
