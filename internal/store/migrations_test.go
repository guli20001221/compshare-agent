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
		"uk_request_uuid", // unique constraint on request_uuid (PostgreSQL: CONSTRAINT ... UNIQUE)
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
	assert.Contains(t, ddl, "ADD COLUMN IF NOT EXISTS context_version INT NOT NULL DEFAULT 0")
	// PostgreSQL appends columns at the end of the row; MySQL's `AFTER context`
	// positional clause has no equivalent and was dropped in the migration.
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
		assert.Contains(t, ddl, "ADD COLUMN IF NOT EXISTS "+column, "0004 must add column %s", column)
	}
	// Columns must be NULLable so an axis that did not fire stores NULL (clean
	// GROUP BY / COUNT semantics) — never NOT NULL with a default.
	assert.NotContains(t, strings.ToUpper(ddl), "NOT NULL")
}

// TestHTTPMigrationsCreateSSHOpsAudit pins the 0011 fail-closed audit table to the exact columns the
// writer (store.SSHOpsAuditStore.Begin/Finish) references and to the INV-9 replay-dedup constraint. A
// drift on either side — a renamed/removed column, or a dropped UNIQUE — silently breaks the audit
// INSERT (failing closed, refusing every diagnosis) or reopens duplicate re-entry.
func TestHTTPMigrationsCreateSSHOpsAudit(t *testing.T) {
	sqlPath := filepath.Join("..", "..", "deploy", "migrations", "0011_create_ssh_ops_audit.sql")
	data, err := os.ReadFile(sqlPath)
	require.NoError(t, err)

	ddl := string(data)
	assert.Contains(t, ddl, "CREATE TABLE IF NOT EXISTS ssh_ops_audit")
	// every column the Begin INSERT / Finish UPDATE names must exist
	for _, column := range []string{
		"id", "request_uuid", "turn_id", "task_hash",
		"top_organization_id", "organization_id", "instance_id", "task", "phase",
		"disposition", "exit_code", "timed_out", "output_bytes", "err_class",
		"started_at", "finished_at",
	} {
		assert.Contains(t, ddl, column, "0011 must define column %s", column)
	}
	// INV-9: the (turn_id, task_hash) UNIQUE is the replay-dedup key. Losing it reopens re-entry.
	assert.Contains(t, ddl, "UNIQUE (turn_id, task_hash)")
	// the crown-jewel secret must never have a column to land in (its field name is "password")
	assert.NotContains(t, strings.ToLower(ddl), "password")
}

// TestHTTPMigrationsAddSSHOpsContextObservability pins the columns introduced
// by 0013. The audit writer references them on every contextual SSH run; a
// missing column would otherwise make the fail-closed lane refuse after deploy.
func TestHTTPMigrationsAddSSHOpsContextObservability(t *testing.T) {
	sqlPath := filepath.Join("..", "..", "deploy", "migrations", "0013_add_ssh_ops_context_observability.sql")
	data, err := os.ReadFile(sqlPath)
	require.NoError(t, err)

	ddl := string(data)
	for _, column := range []string{
		"context_schema_version", "context_fact_coverage", "commands_ran", "commands_refused", "first_command_class",
	} {
		assert.Contains(t, ddl, column, "0013 must define column %s", column)
	}
	assert.NotContains(t, strings.ToLower(ddl), "password")
	assert.NotContains(t, strings.ToLower(ddl), "raw_command")
}

// TestHTTPMigrationsCreateFeishuOAuthTokens pins the schema used by the
// external-group screenshot adapter. Persisted token fields must be ciphertexts:
// a future migration must not accidentally turn the database into a plaintext
// OAuth-token store.
func TestHTTPMigrationsCreateFeishuOAuthTokens(t *testing.T) {
	sqlPath := filepath.Join("..", "..", "deploy", "migrations", "0012_create_feishu_oauth_tokens.sql")
	data, err := os.ReadFile(sqlPath)
	require.NoError(t, err)

	ddl := string(data)
	assert.Contains(t, ddl, "CREATE TABLE IF NOT EXISTS feishu_oauth_tokens")
	for _, column := range []string{
		"purpose", "access_token_ciphertext", "refresh_token_ciphertext",
		"access_expires_at", "refresh_token_expires_at", "updated_at",
	} {
		assert.Contains(t, ddl, column, "0012 must define column %s", column)
	}
	assert.NotContains(t, strings.ToLower(ddl), "access_token TEXT")
	assert.NotContains(t, strings.ToLower(ddl), "refresh_token TEXT")
}
