package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPMigrationsCreateAgentTraces(t *testing.T) {
	sqlPath := filepath.Join("..", "..", "deploy", "migrations", "0002_create_agent_traces.sql")
	data, err := os.ReadFile(sqlPath)
	require.NoError(t, err)

	ddl := string(data)
	assert.Contains(t, ddl, "CREATE TABLE IF NOT EXISTS agent_traces")
	for _, column := range []string{
		"request_uuid",
		"top_organization_id",
		"organization_id",
		"connection_id",
		"trace_json",
		"UNIQUE KEY uk_request_uuid",
	} {
		assert.Contains(t, ddl, column)
	}
	assert.False(t, strings.Contains(strings.ToLower(ddl), "agent_messages"))
}

func TestHTTPMigrationsAddSessionContextVersion(t *testing.T) {
	sqlPath := filepath.Join("..", "..", "deploy", "migrations", "0003_add_session_context_version.sql")
	data, err := os.ReadFile(sqlPath)
	require.NoError(t, err)

	ddl := string(data)
	assert.Contains(t, ddl, "ALTER TABLE sessions")
	assert.Contains(t, ddl, "ADD COLUMN context_version INT NOT NULL DEFAULT 0")
	assert.Contains(t, ddl, "AFTER context")
}

// TestHTTPMigrationsAddAgentTracesOutcomeColumns pins the 0004 promote-to-columns
// migration to the exact column names the MySQL writer's promoted INSERT references
// (mysql_writer.go::promotedInsertCols). A drift on either side — a renamed column in
// the DDL or a reordered value in promotedColumnValues — silently writes the wrong
// axis into the wrong dashboard column, so this guards the DDL↔writer contract.
func TestHTTPMigrationsAddAgentTracesOutcomeColumns(t *testing.T) {
	sqlPath := filepath.Join("..", "..", "deploy", "migrations", "0004_add_agent_traces_outcome_columns.sql")
	data, err := os.ReadFile(sqlPath)
	require.NoError(t, err)

	ddl := string(data)
	assert.Contains(t, ddl, "ALTER TABLE agent_traces")
	for _, column := range []string{
		"terminated_by",
		"abort_cause",
		"error_class",
		"resolution",
		"route_status",
		"refusal_type",
		"resolution_source",
	} {
		assert.Contains(t, ddl, "ADD COLUMN "+column, "0004 must add column %s", column)
	}
	// Columns must be NULLable so an axis that did not fire stores NULL (clean
	// GROUP BY / COUNT semantics) — never NOT NULL with a default.
	assert.NotContains(t, strings.ToUpper(ddl), "NOT NULL")
}
