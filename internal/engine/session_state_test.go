package engine

import (
	"encoding/json"
	"testing"

	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/intent"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newEngineForSessionStateTest constructs a minimal Engine sufficient to
// exercise SessionState methods. It does not wire any LLM-facing dependencies
// because SessionState methods are pure field accessors.
func newEngineForSessionStateTest(t *testing.T) *Engine {
	t.Helper()
	deps := &SharedDeps{
		LLMClient:                &mockLLM{},
		RateLimiter:              governance.NewMemoryLimiter(governance.DefaultLimits()),
		SupportsObjectToolChoice: true,
		ExternalExecutor:         &mockExecutor{results: map[string]map[string]any{}},
	}
	return NewSession(deps, SessionOptions{Subject: "test-subject"})
}

// ---------------------------------------------------------------------------
// SessionState marshal / parse — pure functions on the persisted shape.
// ---------------------------------------------------------------------------

// TestSessionState_MarshalAlwaysIncludesSchemaVersion guards the contract
// that schema_version is always on the wire, even if a zeroed SessionState
// is marshaled. Future readers may key on its presence to detect "empty
// envelope" vs "missing field".
func TestSessionState_MarshalAlwaysIncludesSchemaVersion(t *testing.T) {
	s := SessionState{} // zero value, no SchemaVersion set
	raw, err := json.Marshal(s)
	require.NoError(t, err)
	assert.JSONEq(t, `{"schema_version":"1.0"}`, string(raw))
}

// ---------------------------------------------------------------------------
// PersistedContext envelope round-trip
// ---------------------------------------------------------------------------

func TestPersistedContext_RoundTripBytes(t *testing.T) {
	pc1 := PersistedContext{
		AgentSessionState: SessionState{
			SchemaVersion:        SessionStateSchemaV1,
			SelectedInstanceID:   "uhost-abc123",
			SelectedInstanceName: "gpu-prod-01",
			LastIntent:           string(intent.IntentMonitorQuery),
		},
		ClientContext: json.RawMessage(`{"source":"console","page":"/instance/list"}`),
	}
	raw, err := json.Marshal(pc1)
	require.NoError(t, err)

	pc2, err := ParsePersistedContext(raw)
	require.NoError(t, err)
	assert.Equal(t, pc1.AgentSessionState, pc2.AgentSessionState)
	assert.JSONEq(t, string(pc1.ClientContext), string(pc2.ClientContext))

	raw2, err := json.Marshal(pc2)
	require.NoError(t, err)
	assert.JSONEq(t, string(raw), string(raw2))
}

// TestParsePersistedContext_LegacyObjectWrappedAsClientContext covers a
// pre-M1 object-shaped Context: any object lacking agent_session_state is
// treated as opaque client_context, and a successful chat-turn persist
// will upgrade the row to envelope on the next turn.
func TestParsePersistedContext_LegacyObjectWrappedAsClientContext(t *testing.T) {
	legacy := json.RawMessage(`{"source":"old_client","note":"pre-M1"}`)

	pc, err := ParsePersistedContext(legacy)
	require.NoError(t, err)
	assert.Equal(t, SessionState{SchemaVersion: SessionStateSchemaV1}, pc.AgentSessionState)
	assert.JSONEq(t, string(legacy), string(pc.ClientContext))
}

// TestParsePersistedContext_LegacyNonObjectShapes covers the fact that
// CreateCSAgentSession.Context accepts any valid JSON via optionalJSON
// (internal/httpapi/handlers_session.go:109), not just objects. Arrays,
// strings, numbers, booleans must all be wrapped opaquely into
// client_context, NOT cause a parse error.
func TestParsePersistedContext_LegacyNonObjectShapes(t *testing.T) {
	cases := []json.RawMessage{
		json.RawMessage(`[1,2,3]`),
		json.RawMessage(`"opaque string blob"`),
		json.RawMessage(`42`),
		json.RawMessage(`true`),
		json.RawMessage(`false`),
		json.RawMessage(`{"foo":"bar","nested":{"k":1}}`),
	}
	for _, raw := range cases {
		pc, err := ParsePersistedContext(raw)
		require.NoError(t, err, "input: %s", string(raw))
		assert.Equal(t, SessionState{SchemaVersion: SessionStateSchemaV1}, pc.AgentSessionState,
			"input: %s", string(raw))
		assert.JSONEq(t, string(raw), string(pc.ClientContext), "input: %s", string(raw))
	}
}

func TestParsePersistedContext_NullAndEmpty(t *testing.T) {
	for _, in := range []json.RawMessage{
		nil,
		{},
		json.RawMessage(`null`),
		json.RawMessage(`  null  `),     // whitespace around null
		json.RawMessage("\n  \t\n  \t"), // whitespace only
	} {
		pc, err := ParsePersistedContext(in)
		require.NoError(t, err, "input: %q", string(in))
		assert.Equal(t, SessionState{SchemaVersion: SessionStateSchemaV1}, pc.AgentSessionState)
		assert.Empty(t, pc.ClientContext)
	}
}

func TestParsePersistedContext_MalformedJSON_ReturnsError(t *testing.T) {
	_, err := ParsePersistedContext(json.RawMessage(`{not valid`))
	assert.Error(t, err)
}

// TestParsePersistedContext_LegacyShapesShareAgentKey guards against an
// over-loose probe: a legacy client_context that happens to contain an
// agent_session_state key — but without a recognized envelope shape —
// must be treated as legacy and preserved verbatim. Otherwise commit ④'s
// write-back would silently drop sibling fields of the legacy blob.
func TestParsePersistedContext_LegacyShapesShareAgentKey(t *testing.T) {
	cases := []struct {
		name string
		raw  json.RawMessage
	}{
		{
			name: "agent_session_state present but not an object",
			raw:  json.RawMessage(`{"agent_session_state":"oops","x":1}`),
		},
		{
			name: "agent_session_state object without schema_version",
			raw:  json.RawMessage(`{"agent_session_state":{"foo":"bar"},"x":1}`),
		},
		{
			name: "agent_session_state.schema_version is not a string",
			raw:  json.RawMessage(`{"agent_session_state":{"schema_version":1},"x":1}`),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pc, err := ParsePersistedContext(tc.raw)
			require.NoError(t, err)
			// Must be treated as legacy: empty agent state, whole blob preserved.
			assert.Equal(t, SessionState{SchemaVersion: SessionStateSchemaV1}, pc.AgentSessionState,
				"strict detection should NOT have claimed this as an envelope")
			assert.JSONEq(t, string(tc.raw), string(pc.ClientContext),
				"legacy blob must be preserved verbatim as client_context")
		})
	}
}

// TestParsePersistedContext_UnknownSchemaVersion_ReturnsTypedError covers
// the forward-rollout scenario: a v2 binary writes top-level v2 envelope,
// then an older v1 binary on the same row must NOT silently downgrade by
// stuffing the v2 envelope into client_context (that would lose the v2
// state on the next v2 read). Instead the v1 binary returns
// ErrUnknownSessionStateSchema, the http layer skips persistence, and the
// row is left untouched for the v2 binary to read correctly.
func TestParsePersistedContext_UnknownSchemaVersion_ReturnsTypedError(t *testing.T) {
	cases := []json.RawMessage{
		json.RawMessage(`{"agent_session_state":{"schema_version":"0.0"},"x":1}`),
		json.RawMessage(`{"agent_session_state":{"schema_version":"2.0","new_field":"hello"},"client_context":{"app":"console"}}`),
		json.RawMessage(`{"agent_session_state":{"schema_version":"v9-beta"}}`),
	}
	for _, raw := range cases {
		pc, err := ParsePersistedContext(raw)
		assert.ErrorIs(t, err, ErrUnknownSessionStateSchema, "input: %s", string(raw))
		// On error the PersistedContext is zero — caller must not persist.
		assert.Equal(t, PersistedContext{}, pc, "input: %s", string(raw))
	}
}

// TestParsePersistedContext_RecognizesKnownSchemaVersion is the positive
// counterpart to the strict-detection tests. An envelope whose
// schema_version is in knownSessionStateSchemaVersions must be decoded as
// the real agent state.
func TestParsePersistedContext_RecognizesKnownSchemaVersion(t *testing.T) {
	raw := json.RawMessage(`{"agent_session_state":{"schema_version":"1.0","selected_instance_id":"u-1"},"client_context":{"app":"console"}}`)
	pc, err := ParsePersistedContext(raw)
	require.NoError(t, err)
	assert.Equal(t, SessionStateSchemaV1, pc.AgentSessionState.SchemaVersion)
	assert.Equal(t, "u-1", pc.AgentSessionState.SelectedInstanceID)
	assert.JSONEq(t, `{"app":"console"}`, string(pc.ClientContext))
}

// ---------------------------------------------------------------------------
// LastIntent vocabulary contract
// ---------------------------------------------------------------------------

// TestSessionState_LastIntentValuesMatchIntentEnum enforces that any
// future writer that sets LastIntent uses the exact intent.Intent string
// values, not short aliases. The engine package stays decoupled from the
// intent package at compile time (no production-code import); this test
// imports intent only to assemble the legal-value set.
func TestSessionState_LastIntentValuesMatchIntentEnum(t *testing.T) {
	legal := map[string]struct{}{"": {}}
	for _, it := range intent.RuntimeIntents() {
		legal[string(it)] = struct{}{}
	}

	// Spot-check several intents we expect to flow through SessionState writers.
	assert.Contains(t, legal, string(intent.IntentMonitorQuery))
	assert.Contains(t, legal, string(intent.IntentResourceInfo))
	assert.Contains(t, legal, string(intent.IntentGPUSpecsQuery))
	assert.Contains(t, legal, string(intent.IntentPricingQuery))

	// Reject hypothetical short aliases that might leak in from informal usage.
	for _, alias := range []string{"monitor", "resource", "gpu_specs", "pricing"} {
		assert.NotContains(t, legal, alias,
			"short alias %q must NOT be a legal LastIntent value", alias)
	}
}

// ---------------------------------------------------------------------------
// Engine inject / export / clear
// ---------------------------------------------------------------------------

func TestEngine_SessionStateSnapshot_DefaultsToUnhydrated(t *testing.T) {
	e := newEngineForSessionStateTest(t)
	s, ver, hydrated := e.SessionStateSnapshot()
	assert.False(t, hydrated, "fresh Engine must NOT be hydrated before SetSessionState")
	assert.Equal(t, SessionState{}, s)
	assert.Equal(t, 0, ver)
}

func TestEngine_SetSessionState_RoundTrip(t *testing.T) {
	e := newEngineForSessionStateTest(t)

	s1 := SessionState{
		SchemaVersion:      SessionStateSchemaV1,
		SelectedInstanceID: "uhost-xyz",
		LastIntent:         string(intent.IntentResourceInfo),
	}
	e.SetSessionState(s1, 7)

	s2, ver, hydrated := e.SessionStateSnapshot()
	assert.True(t, hydrated)
	assert.Equal(t, s1, s2)
	assert.Equal(t, 7, ver)
}

// TestEngine_ClearSessionState_ResetsHydrated guards the cached-Engine
// reuse bug: agentpool reuses *engine.Engine across turns. Without an
// explicit clear, turn N+1's parse failure would inherit turn N's
// hydrated=true and cause the persist-on-success path to overwrite stale
// state on top of the broken row.
func TestEngine_ClearSessionState_ResetsHydrated(t *testing.T) {
	e := newEngineForSessionStateTest(t)
	e.SetSessionState(SessionState{
		SchemaVersion:      SessionStateSchemaV1,
		SelectedInstanceID: "uhost-prev",
		LastIntent:         string(intent.IntentMonitorQuery),
	}, 3)

	s, ver, hydrated := e.SessionStateSnapshot()
	require.True(t, hydrated)
	require.Equal(t, "uhost-prev", s.SelectedInstanceID)
	require.Equal(t, 3, ver)

	e.ClearSessionState()

	s, ver, hydrated = e.SessionStateSnapshot()
	assert.False(t, hydrated)
	assert.Equal(t, SessionState{}, s)
	assert.Equal(t, 0, ver)
}

// TestEngine_RoundTrip_AcrossReplicas is the core multi-replica preservation
// contract: replica A's SessionState, after marshal-into-envelope and
// re-parse on replica B, must produce a byte-equal state on B. Any field
// that breaks this (pointers, cache handles, unexported state) is forbidden.
func TestEngine_RoundTrip_AcrossReplicas(t *testing.T) {
	eA := newEngineForSessionStateTest(t)
	eA.SetSessionState(SessionState{
		SchemaVersion:      SessionStateSchemaV1,
		SelectedInstanceID: "uhost-roundtrip",
		LastIntent:         string(intent.IntentGPUSpecsQuery),
	}, 0)

	stateA, _, _ := eA.SessionStateSnapshot()

	// Wrap in envelope as handleChat would do on persist.
	raw, err := json.Marshal(PersistedContext{AgentSessionState: stateA})
	require.NoError(t, err)

	// Replica B: rehydrate from DB.
	pcB, err := ParsePersistedContext(raw)
	require.NoError(t, err)
	eB := newEngineForSessionStateTest(t)
	eB.SetSessionState(pcB.AgentSessionState, 1)

	got, ver, hydrated := eB.SessionStateSnapshot()
	assert.True(t, hydrated)
	assert.Equal(t, stateA, got)
	assert.Equal(t, 1, ver)
}

// TestEnvelope_PreservesClientContextAcrossAgentWrites guards the public
// CreateCSAgentSession.Context API contract: client_context written by the
// frontend must survive agent-side SessionState updates.
func TestEnvelope_PreservesClientContextAcrossAgentWrites(t *testing.T) {
	clientCtx := json.RawMessage(`{"app":"console","theme":"dark"}`)

	pcWithClient := PersistedContext{
		AgentSessionState: SessionState{SchemaVersion: SessionStateSchemaV1},
		ClientContext:     clientCtx,
	}
	raw, err := json.Marshal(pcWithClient)
	require.NoError(t, err)

	pcRead, err := ParsePersistedContext(raw)
	require.NoError(t, err)
	assert.JSONEq(t, string(clientCtx), string(pcRead.ClientContext))

	// Agent updates its state, rewrites envelope.
	pcRead.AgentSessionState.SelectedInstanceID = "uhost-x"
	raw2, err := json.Marshal(pcRead)
	require.NoError(t, err)

	pcFinal, err := ParsePersistedContext(raw2)
	require.NoError(t, err)
	assert.Equal(t, "uhost-x", pcFinal.AgentSessionState.SelectedInstanceID)
	assert.JSONEq(t, string(clientCtx), string(pcFinal.ClientContext))
}
