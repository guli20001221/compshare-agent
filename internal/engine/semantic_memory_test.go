package engine

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLegacySemanticStateIsDroppedOnRewrite(t *testing.T) {
	raw := json.RawMessage(`{
  "agent_session_state": {
    "schema_version": "7.0",
    "context_frame": {"workflow":"CreateDiskWorkflow"},
    "task_snapshot": {"goal":"扩盘"},
    "conversation_digest": {"narrative":"旧摘要"},
    "recent_facts": [{"kind":"instance_state","subject_id":"uhost-a","payload":{"state":"Running"},"produced_at_unix":1,"ttl_seconds":300}]
  }
}`)
	parsed, err := ParsePersistedContext(raw)
	require.NoError(t, err)

	rewritten, err := json.Marshal(parsed.AgentSessionState)
	require.NoError(t, err)
	assert.NotContains(t, string(rewritten), "context_frame")
	assert.NotContains(t, string(rewritten), "task_snapshot")
	assert.NotContains(t, string(rewritten), "conversation_digest")
	assert.NotContains(t, string(rewritten), "recent_facts")
	assert.NotContains(t, string(rewritten), "payload")
}
