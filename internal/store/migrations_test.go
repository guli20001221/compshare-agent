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
	assert.Contains(t, ddl, "ADD COLUMN context_version INT NOT NULL DEFAULT 0")
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
		assert.Contains(t, ddl, "ADD COLUMN "+column, "0004 must add column %s", column)
	}
	// Columns must be NULLable so an axis that did not fire stores NULL (clean
	// GROUP BY / COUNT semantics) — never NOT NULL with a default.
	assert.NotContains(t, strings.ToUpper(ddl), "NOT NULL")
}

func TestHTTPMigrationsCreateTurnExecutionKernel(t *testing.T) {
	sqlPath := filepath.Join("..", "..", "deploy", "migrations", "0005_create_turn_execution.sql")
	data, err := os.ReadFile(sqlPath)
	require.NoError(t, err)

	ddl := string(data)
	for _, fragment := range []string{
		"CREATE TABLE chat_turns",
		"CREATE TABLE conversation_leases",
		"CREATE TABLE turn_actions",
		"ADD COLUMN turn_id",
		"ADD COLUMN turn_role",
		"UNIQUE (top_organization_id, organization_id, session_id, client_turn_id)",
		"PRIMARY KEY (turn_id, action_index)",
		"active_turn_id",
		"next_event_seq",
		"turn_seq",
		"UNIQUE (session_id, turn_seq)",
		"UNIQUE (turn_id, action_name, args_hash)",
		"action_name",
		"args_hash",
		"in_flight",
		"upstream_request_id",
		"in_flight        BOOLEAN       NOT NULL DEFAULT FALSE",
		"status = 'reserved' OR NOT in_flight",
	} {
		assert.Contains(t, ddl, fragment)
	}
}

func TestHTTPMigrationsCreateTurnProtocol(t *testing.T) {
	sqlPath := filepath.Join("..", "..", "deploy", "migrations", "0006_create_turn_protocol.sql")
	data, err := os.ReadFile(sqlPath)
	require.NoError(t, err)

	ddl := string(data)
	for _, fragment := range []string{
		"CREATE TABLE chat_turn_events",
		"CREATE TABLE turn_interactions",
		"PRIMARY KEY (turn_id, seq)",
		"UNIQUE (turn_id, interaction_key)",
		"provisional",
		"lease_epoch",
		"expires_at",
		"resolution_hash",
	} {
		assert.Contains(t, ddl, fragment)
	}
	assert.NotContains(t, ddl, "response_hash", "one canonical resolution hash avoids contradictory duplicate fields")
}

func TestHTTPMigrationsAddTurnRecoveryContext(t *testing.T) {
	sqlPath := filepath.Join("..", "..", "deploy", "migrations", "0007_add_turn_recovery_context.sql")
	data, err := os.ReadFile(sqlPath)
	require.NoError(t, err)

	ddl := string(data)
	for _, fragment := range []string{
		"ADD COLUMN execution_envelope JSONB",
		"ADD COLUMN context_hint JSONB",
		"context_hint - 'resource_ids' - 'region' - 'zone'",
		"idx_chat_turns_recovery",
		"'awaiting_confirmation'",
		"'committing'",
	} {
		assert.Contains(t, ddl, fragment)
	}
	assert.NotContains(t, ddl, "execution_envelope JSONB NOT NULL", "existing turns must remain valid")
	assert.NotContains(t, ddl, "context_hint JSONB NOT NULL", "existing actions must remain valid")
}
