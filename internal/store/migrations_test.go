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
