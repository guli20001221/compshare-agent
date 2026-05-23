package engine

import (
	"encoding/json"
	"fmt"
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

// ---------------------------------------------------------------------------
// M2 — RecentFacts / ToolFact round-trip + helpers
// ---------------------------------------------------------------------------

// TestSessionState_RoundTripWithRecentFacts extends the M1 byte-equal
// envelope round-trip to include a non-empty RecentFacts slice. Any field
// added to ToolFact whose serialization is not stable (pointer, time.Time,
// unexported) breaks this.
func TestSessionState_RoundTripWithRecentFacts(t *testing.T) {
	pc1 := PersistedContext{
		AgentSessionState: SessionState{
			SchemaVersion:        SessionStateSchemaV1,
			SelectedInstanceID:   "uhost-abc",
			SelectedInstanceName: "gpu-prod",
			LastIntent:           string(intent.IntentMonitorQuery),
			RecentFacts: []ToolFact{
				{
					Kind:           FactKindInstanceState,
					SubjectID:      "uhost-abc",
					Payload:        map[string]any{"name": "gpu-prod", "state": "Running", "gpu": float64(2), "gpu_type": "RTX4090", "cpu": float64(16), "memory": float64(64), "zone": "cn-bj-01"},
					ProducedAtTurn: 3,
					ProducedAtUnix: 1716530000,
					TTLSeconds:     factTTLSecondsInstanceState,
				},
				{
					Kind:           FactKindMonitorSample,
					SubjectID:      "uhost-abc",
					Payload:        map[string]any{"cpu_usage": "92.5", "memory_usage": "44.0"},
					ProducedAtTurn: 3,
					ProducedAtUnix: 1716530002,
					TTLSeconds:     factTTLSecondsMonitorSample,
				},
			},
		},
	}
	raw, err := json.Marshal(pc1)
	require.NoError(t, err)

	pc2, err := ParsePersistedContext(raw)
	require.NoError(t, err)
	require.Equal(t, pc1.AgentSessionState.SchemaVersion, pc2.AgentSessionState.SchemaVersion)
	require.Equal(t, pc1.AgentSessionState.SelectedInstanceID, pc2.AgentSessionState.SelectedInstanceID)
	require.Equal(t, pc1.AgentSessionState.LastIntent, pc2.AgentSessionState.LastIntent)
	require.Len(t, pc2.AgentSessionState.RecentFacts, 2)

	raw2, err := json.Marshal(pc2)
	require.NoError(t, err)
	assert.JSONEq(t, string(raw), string(raw2),
		"byte-equal round-trip required for multi-replica preservation")
}

// TestToolFact_PayloadKeysPerKind enforces the documented payload-key set
// per fact kind. Future writers must use exactly these keys; M3
// ContextAssembler will read them by name. Adding a new key requires
// updating this test deliberately.
func TestToolFact_PayloadKeysPerKind(t *testing.T) {
	expected := map[string]map[string]struct{}{
		FactKindInstanceState: {
			"name":     {},
			"state":    {},
			"gpu":      {},
			"gpu_type": {},
			"cpu":      {},
			"memory":   {},
			"zone":     {},
		},
		FactKindMonitorSample: {
			// Renderer-derived metric keys at internal/intent/envelope.go.
			// Multi-GPU disambiguation produces gpu_usage.GPU 1 / .GPU 2;
			// they share the same fact via the dotted-suffix convention.
			"cpu_usage":         {},
			"memory_usage":      {},
			"gpu_usage":         {},
			"vram_usage":        {},
			"system_disk_usage": {},
			"data_disk_usage":   {},
		},
	}

	// Sanity: every supported kind has a TTL constant and an entry in
	// ttlSecondsForKind.
	for kind := range expected {
		ttl := ttlSecondsForKind(kind)
		assert.Greaterf(t, ttl, 0, "kind %q must have a non-zero default TTL", kind)
	}
	assert.Equal(t, 0, ttlSecondsForKind("unknown_kind"),
		"unknown kinds must default to TTL=0 so M3 assembler treats them as expired")
}

// TestAppendFactToSlice_DedupesBySubjectAndKind covers the (Kind, SubjectID)
// dedupe contract: a second fact with the same key replaces the first when
// its ProducedAtUnix is newer-or-equal; older facts are kept.
func TestAppendFactToSlice_DedupesBySubjectAndKind(t *testing.T) {
	base := []ToolFact{
		{Kind: FactKindInstanceState, SubjectID: "uhost-A", ProducedAtUnix: 100, Payload: map[string]any{"state": "Running"}},
		{Kind: FactKindInstanceState, SubjectID: "uhost-B", ProducedAtUnix: 110, Payload: map[string]any{"state": "Stopped"}},
	}
	out := appendFactToSlice(base, ToolFact{
		Kind: FactKindInstanceState, SubjectID: "uhost-A", ProducedAtUnix: 200,
		Payload: map[string]any{"state": "Stopped"},
	})
	require.Len(t, out, 2)

	// Find uhost-A — newer fact wins.
	var uhostA *ToolFact
	for i := range out {
		if out[i].SubjectID == "uhost-A" {
			uhostA = &out[i]
		}
	}
	require.NotNil(t, uhostA)
	assert.EqualValues(t, 200, uhostA.ProducedAtUnix)
	assert.Equal(t, "Stopped", uhostA.Payload["state"])
}

// TestAppendFactToSlice_OlderFactDoesNotOverwrite covers the
// "newer wins" rule: an append with a stale ProducedAtUnix must NOT
// downgrade the existing fact. Required for multi-replica preservation
// per [[project-multi-replica-interfaces]] §2.
func TestAppendFactToSlice_OlderFactDoesNotOverwrite(t *testing.T) {
	base := []ToolFact{
		{Kind: FactKindInstanceState, SubjectID: "uhost-A", ProducedAtUnix: 200, Payload: map[string]any{"state": "Running"}},
	}
	out := appendFactToSlice(base, ToolFact{
		Kind: FactKindInstanceState, SubjectID: "uhost-A", ProducedAtUnix: 100,
		Payload: map[string]any{"state": "Stopped"}, // stale
	})
	require.Len(t, out, 1)
	assert.EqualValues(t, 200, out[0].ProducedAtUnix, "older ProducedAtUnix must NOT clobber newer fact")
	assert.Equal(t, "Running", out[0].Payload["state"])
}

// TestAppendFactToSlice_RespectsCap caps RecentFacts at maxRecentFacts.
// Oldest by ProducedAtUnix is dropped. Mutation test reference: changing
// the cap or removing the cap branch makes this test fail.
func TestAppendFactToSlice_RespectsCap(t *testing.T) {
	var facts []ToolFact
	// Insert maxRecentFacts+5 distinct subjects so dedupe never fires.
	for i := 0; i < maxRecentFacts+5; i++ {
		facts = appendFactToSlice(facts, ToolFact{
			Kind:           FactKindInstanceState,
			SubjectID:      fmt.Sprintf("uhost-%02d", i),
			ProducedAtUnix: int64(1_000_000 + i), // monotonically newer
		})
	}
	assert.Len(t, facts, maxRecentFacts, "slice must be capped at maxRecentFacts")

	// Oldest entries (lowest ProducedAtUnix) are dropped; newest survive.
	for _, f := range facts {
		assert.GreaterOrEqual(t, f.ProducedAtUnix, int64(1_000_005),
			"capped slice should keep the 16 newest, dropping the 5 oldest")
	}
}

// TestMergeFactsByProducedAt_KeepsHigherTimestamp covers the multi-replica
// reconciliation: when local and incoming both have a fact for the same
// (Kind, SubjectID), the higher ProducedAtUnix wins. Used by version-aware
// merge in M2 commit 5.
func TestMergeFactsByProducedAt_KeepsHigherTimestamp(t *testing.T) {
	local := []ToolFact{
		{Kind: FactKindInstanceState, SubjectID: "uhost-A", ProducedAtUnix: 200},
		{Kind: FactKindMonitorSample, SubjectID: "uhost-A", ProducedAtUnix: 210},
	}
	incoming := []ToolFact{
		{Kind: FactKindInstanceState, SubjectID: "uhost-A", ProducedAtUnix: 100}, // older
		{Kind: FactKindInstanceState, SubjectID: "uhost-B", ProducedAtUnix: 220}, // unique
	}
	out := mergeFactsByProducedAt(local, incoming)
	require.Len(t, out, 3)

	// Index by (kind, subject) for assertions.
	byKey := make(map[string]ToolFact, len(out))
	for _, f := range out {
		byKey[f.Kind+":"+f.SubjectID] = f
	}
	assert.EqualValues(t, 200, byKey[FactKindInstanceState+":uhost-A"].ProducedAtUnix,
		"local newer must win when incoming is older")
	assert.EqualValues(t, 210, byKey[FactKindMonitorSample+":uhost-A"].ProducedAtUnix)
	assert.EqualValues(t, 220, byKey[FactKindInstanceState+":uhost-B"].ProducedAtUnix)
}

// TestMergeFactsByProducedAt_DoesNotMutateInputs ensures the merge function
// is pure — neither input slice is modified. Required because the engine's
// in-memory state is one of the inputs and must not be mutated by a stale
// hydrate side-effect.
func TestMergeFactsByProducedAt_DoesNotMutateInputs(t *testing.T) {
	local := []ToolFact{
		{Kind: FactKindInstanceState, SubjectID: "uhost-A", ProducedAtUnix: 200},
	}
	incoming := []ToolFact{
		{Kind: FactKindInstanceState, SubjectID: "uhost-A", ProducedAtUnix: 100},
	}
	localCopy := append([]ToolFact(nil), local...)
	incomingCopy := append([]ToolFact(nil), incoming...)

	_ = mergeFactsByProducedAt(local, incoming)

	assert.Equal(t, localCopy, local, "local input must not be mutated")
	assert.Equal(t, incomingCopy, incoming, "incoming input must not be mutated")
}

// TestMergeFactsByProducedAt_RespectsCap exercises the cap on the output of
// the merge path. Same constant as appendFactToSlice; same drop-oldest
// policy.
func TestMergeFactsByProducedAt_RespectsCap(t *testing.T) {
	build := func(prefix string, base int64, n int) []ToolFact {
		out := make([]ToolFact, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, ToolFact{
				Kind:           FactKindInstanceState,
				SubjectID:      fmt.Sprintf("%s-%02d", prefix, i),
				ProducedAtUnix: base + int64(i),
			})
		}
		return out
	}
	// 12 + 12 = 24 facts; cap=16, so 8 oldest get dropped.
	// All R-batch (2_000_000+i) are newer than all L-batch (1_000_000+i),
	// so survivors are 12×R + the 4 newest of L (L-11/10/09/08).
	local := build("L", 1_000_000, 12)
	incoming := build("R", 2_000_000, 12)
	out := mergeFactsByProducedAt(local, incoming)
	assert.Len(t, out, maxRecentFacts)

	// Verify every R survived; verify dropped L entries are L-00..L-07.
	survivorIDs := make(map[string]struct{}, len(out))
	for _, f := range out {
		survivorIDs[f.SubjectID] = struct{}{}
	}
	for i := 0; i < 12; i++ {
		_, ok := survivorIDs[fmt.Sprintf("R-%02d", i)]
		assert.Truef(t, ok, "R-%02d must survive (it's newer than every L)", i)
	}
	for _, expected := range []string{"L-08", "L-09", "L-10", "L-11"} {
		_, ok := survivorIDs[expected]
		assert.Truef(t, ok, "%s should be in survivors (top-4 of L by ProducedAtUnix)", expected)
	}
	for _, dropped := range []string{"L-00", "L-01", "L-02", "L-03", "L-04", "L-05", "L-06", "L-07"} {
		_, ok := survivorIDs[dropped]
		assert.Falsef(t, ok, "%s should have been dropped (oldest 8 of L)", dropped)
	}
}
