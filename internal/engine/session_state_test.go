package engine

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/knowledge"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newEngineForSessionStateTest(t *testing.T) *Engine {
	t.Helper()
	return NewSession(&SharedDeps{
		LLMClient:        &mockLLM{},
		RateLimiter:      governance.NewInMemoryRateLimiter(governance.DefaultLimits()),
		ExternalExecutor: &mockExecutor{results: map[string]map[string]any{}},
	}, SessionOptions{Subject: "test-subject"})
}

func TestSessionStateMarshalAlwaysIncludesCurrentSchema(t *testing.T) {
	raw, err := json.Marshal(SessionState{})
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprintf(`{"schema_version":%q}`, SessionStateSchemaCurrent), string(raw))
}

func TestPersistedContextPreservesOpaqueClientContext(t *testing.T) {
	client := json.RawMessage(`{"source":"console","page":"/instance/list"}`)
	input := PersistedContext{
		AgentSessionState: SessionState{SchemaVersion: SessionStateSchemaCurrent, SelectedInstanceID: "uhost-a"},
		ClientContext:     client,
	}
	raw, err := json.Marshal(input)
	require.NoError(t, err)
	parsed, err := ParsePersistedContext(raw)
	require.NoError(t, err)
	assert.Equal(t, input.AgentSessionState, parsed.AgentSessionState)
	assert.JSONEq(t, string(client), string(parsed.ClientContext))

	// Client-owned data survives a later agent state update.
	parsed.AgentSessionState.SelectedInstanceID = "uhost-b"
	rewritten, err := json.Marshal(parsed)
	require.NoError(t, err)
	parsedAgain, err := ParsePersistedContext(rewritten)
	require.NoError(t, err)
	assert.Equal(t, "uhost-b", parsedAgain.AgentSessionState.SelectedInstanceID)
	assert.JSONEq(t, string(client), string(parsedAgain.ClientContext))
}

func TestParsePersistedContextTreatsNonEnvelopesAsOpaqueClientState(t *testing.T) {
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"source":"old_client"}`),
		json.RawMessage(`[1,2,3]`),
		json.RawMessage(`"opaque"`),
		json.RawMessage(`42`),
		json.RawMessage(`true`),
		json.RawMessage(`{"agent_session_state":{"schema_version":1}}`),
	} {
		parsed, err := ParsePersistedContext(raw)
		require.NoError(t, err, "input: %s", raw)
		assert.Equal(t, SessionStateSchemaCurrent, parsed.AgentSessionState.SchemaVersion)
		assert.JSONEq(t, string(raw), string(parsed.ClientContext))
	}
}

func TestParsePersistedContextRejectsMalformedAndUnknownEnvelopes(t *testing.T) {
	_, err := ParsePersistedContext(json.RawMessage(`{not json`))
	require.Error(t, err)

	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"agent_session_state":{"schema_version":"0.0"}}`),
		json.RawMessage(`{"agent_session_state":{"schema_version":"9.0","future":"value"}}`),
	} {
		parsed, err := ParsePersistedContext(raw)
		assert.ErrorIs(t, err, ErrUnknownSessionStateSchema)
		assert.Equal(t, PersistedContext{}, parsed)
	}
}

func TestLegacySemanticFieldsDecodeButNeverWriteBack(t *testing.T) {
	raw := json.RawMessage(`{
  "agent_session_state": {
    "schema_version":"4.0",
    "selected_instance_id":"uhost-a",
    "selected_instance_source":"user_selected",
    "context_frame":{"workflow":"CreateDiskWorkflow"},
    "task_snapshot":{"goal":"扩盘"},
    "conversation_digest":{"narrative":"旧摘要"},
    "recent_facts":[{"kind":"instance_state","subject_id":"uhost-a","produced_at_unix":100,"ttl_seconds":300,"payload":{"state":"Running"}}]
  },
  "client_context":{"page":"/instance"}
}`)
	parsed, err := ParsePersistedContext(raw)
	require.NoError(t, err)
	assert.Equal(t, "uhost-a", parsed.AgentSessionState.SelectedInstanceID)

	rewritten, err := json.Marshal(parsed)
	require.NoError(t, err)
	assert.NotContains(t, string(rewritten), "context_frame")
	assert.NotContains(t, string(rewritten), "task_snapshot")
	assert.NotContains(t, string(rewritten), "conversation_digest")
	assert.NotContains(t, string(rewritten), "recent_facts")
	assert.NotContains(t, string(rewritten), "payload")
	assert.Contains(t, string(rewritten), "user_selected")
}

func TestVerifiedKnowledgePersistsEvidenceNotAnswerText(t *testing.T) {
	state := SessionState{SchemaVersion: SessionStateSchemaCurrent, VerifiedKnowledge: []VerifiedKnowledgeTurn{{
		Question: "终端怎么粘贴",
		Evidence: knowledge.EvidenceLedger{Query: "终端怎么粘贴", Items: []knowledge.EvidenceItem{{
			ChunkID: "terminal-paste-001", Title: "终端粘贴", Snippet: "使用 Ctrl+Shift+V 粘贴",
		}}},
		VerifiedAtUnix: 1716530100,
	}}}
	raw, err := json.Marshal(PersistedContext{AgentSessionState: state})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"answer"`)
	parsed, err := ParsePersistedContext(raw)
	require.NoError(t, err)
	assert.Equal(t, state, parsed.AgentSessionState)
}

func TestSessionStateSnapshotAndClear(t *testing.T) {
	e := newEngineForSessionStateTest(t)
	state, version, hydrated := e.SessionStateSnapshot()
	assert.False(t, hydrated)
	assert.Equal(t, SessionState{}, state)
	assert.Zero(t, version)

	e.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaV1, SelectedInstanceID: "uhost-a"}, 3)
	state, version, hydrated = e.SessionStateSnapshot()
	assert.True(t, hydrated)
	assert.Equal(t, SessionStateSchemaCurrent, state.SchemaVersion)
	assert.Equal(t, "uhost-a", state.SelectedInstanceID)
	assert.Equal(t, 3, version)

	e.ClearSessionState()
	state, version, hydrated = e.SessionStateSnapshot()
	assert.False(t, hydrated)
	assert.Equal(t, SessionState{}, state)
	assert.Zero(t, version)
}

func TestSetSessionStateStaleMergeDoesNotClobberLiveSelection(t *testing.T) {
	e := newEngineForSessionStateTest(t)
	e.SetSessionState(SessionState{
		SchemaVersion:        SessionStateSchemaCurrent,
		SelectedInstanceID:   "uhost-local",
		SelectedInstanceName: "local",
	}, 5)

	e.SetSessionState(SessionState{
		SchemaVersion:        SessionStateSchemaCurrent,
		SelectedInstanceID:   "uhost-stale",
		SelectedInstanceName: "stale",
	}, 5)

	state, version, hydrated := e.SessionStateSnapshot()
	require.True(t, hydrated)
	assert.Equal(t, 5, version)
	assert.Equal(t, "uhost-local", state.SelectedInstanceID)
}

func TestSetSessionStateHigherVersionOverwrites(t *testing.T) {
	e := newEngineForSessionStateTest(t)
	e.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent, SelectedInstanceID: "uhost-old"}, 3)
	e.SetSessionState(SessionState{SchemaVersion: SessionStateSchemaCurrent, SelectedInstanceID: "uhost-new"}, 4)
	state, version, _ := e.SessionStateSnapshot()
	assert.Equal(t, "uhost-new", state.SelectedInstanceID)
	assert.Equal(t, 4, version)
}

func TestPendingSelectionRoundTripsAsExecutionState(t *testing.T) {
	state := SessionState{
		SchemaVersion:                  SessionStateSchemaCurrent,
		PendingSelectionKind:           "instance",
		PendingSelectionProducedAtUnix: 1716530001,
		PendingSelectionTTLSeconds:     pendingSelectionTTLSeconds,
		PendingSelectionItems: []PendingSelectionItem{{
			Index: 1, ID: "uhost-list-1", Name: "list-one", State: "Running", GPU: 1, GpuType: "4090", Zone: "cn-wlcb-01",
		}},
	}
	raw, err := json.Marshal(PersistedContext{AgentSessionState: state})
	require.NoError(t, err)
	parsed, err := ParsePersistedContext(raw)
	require.NoError(t, err)
	assert.Equal(t, state, parsed.AgentSessionState)
}
