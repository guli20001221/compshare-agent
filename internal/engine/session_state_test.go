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

// TestSetSessionState_VersionAwareMerge_StaleIncomingDoesNotClobber is the
// load-bearing test for the M2 version-aware merge. It pins the M1
// forward-note (docs/agent/plan/m1-session-state-cas.md:429) — a
// stale-or-equal-version incoming state must NOT overwrite the engine's
// in-memory scalar fields, but its RecentFacts ARE reconciled via
// mergeFactsByProducedAt.
//
// MUTATION CHECK: removing the `version <= e.sessionStateVersion` guard
// from SetSessionState (so the function always overwrites) makes this
// test fail at the SelectedInstance/LastIntent assertions — proven via
// mutation experiment during commit 6.
func TestSetSessionState_VersionAwareMerge_StaleIncomingDoesNotClobber(t *testing.T) {
	e := newEngineForSessionStateTest(t)

	// Initial hydrate at version=5 with one fact.
	e.SetSessionState(SessionState{
		SchemaVersion:        SessionStateSchemaV1,
		SelectedInstanceID:   "uhost-A",
		SelectedInstanceName: "train-a",
		LastIntent:           string(intent.IntentMonitorQuery),
		RecentFacts: []ToolFact{
			{Kind: FactKindInstanceState, SubjectID: "uhost-A", ProducedAtUnix: 200,
				Payload: map[string]any{"state": "Running"}},
		},
	}, 5)

	// Simulate mid-turn writer adding a newer fact for the same subject
	// and a fact for a different kind.
	e.sessionState.RecentFacts = appendFactToSlice(e.sessionState.RecentFacts, ToolFact{
		Kind: FactKindMonitorSample, SubjectID: "uhost-A", ProducedAtUnix: 210,
		Payload: map[string]any{"cpu_usage": "90"},
	})

	// Stale incoming (same version, e.g. cross-replica refetch). Scalars
	// would clobber if the guard were missing.
	e.SetSessionState(SessionState{
		SchemaVersion:        SessionStateSchemaV1,
		SelectedInstanceID:   "uhost-B-stale",  // MUST NOT clobber
		SelectedInstanceName: "stale-name",     // MUST NOT clobber
		LastIntent:           string(intent.IntentResourceInfo), // MUST NOT clobber
		RecentFacts: []ToolFact{
			{Kind: FactKindInstanceState, SubjectID: "uhost-A", ProducedAtUnix: 100,
				Payload: map[string]any{"state": "OLDER"}}, // older — must lose
			{Kind: FactKindInstanceState, SubjectID: "uhost-C", ProducedAtUnix: 220,
				Payload: map[string]any{"state": "Running"}}, // unique — must merge
		},
	}, 5) // SAME VERSION

	state, ver, hydrated := e.SessionStateSnapshot()
	require.True(t, hydrated)
	assert.Equal(t, 5, ver, "version stays at the in-memory value when merge fires")

	// Scalars unchanged.
	assert.Equal(t, "uhost-A", state.SelectedInstanceID,
		"stale incoming MUST NOT clobber SelectedInstanceID — the engine's in-memory scalar is at-or-newer than the row")
	assert.Equal(t, "train-a", state.SelectedInstanceName)
	assert.Equal(t, string(intent.IntentMonitorQuery), state.LastIntent)

	// Facts merged: uhost-A instance_state stays at 200 (local newer than
	// stale incoming's 100); uhost-A monitor_sample stays at 210; uhost-C
	// instance_state is new.
	byKey := make(map[string]ToolFact, len(state.RecentFacts))
	for _, f := range state.RecentFacts {
		byKey[f.Kind+":"+f.SubjectID] = f
	}
	assert.Len(t, state.RecentFacts, 3, "expect 3 merged facts (uhost-A inst, uhost-A mon, uhost-C inst)")
	assert.EqualValues(t, 200, byKey[FactKindInstanceState+":uhost-A"].ProducedAtUnix,
		"local newer instance_state must NOT be downgraded by stale incoming")
	assert.Equal(t, "Running", byKey[FactKindInstanceState+":uhost-A"].Payload["state"])
	assert.EqualValues(t, 210, byKey[FactKindMonitorSample+":uhost-A"].ProducedAtUnix)
	assert.EqualValues(t, 220, byKey[FactKindInstanceState+":uhost-C"].ProducedAtUnix,
		"unique-key incoming fact must be merged in")
}

// TestSetSessionState_HigherVersionOverwrites covers the normal hydrate
// path: when incoming version is strictly greater than in-memory, full
// overwrite happens (the in-memory state was stale).
func TestSetSessionState_HigherVersionOverwrites(t *testing.T) {
	e := newEngineForSessionStateTest(t)

	e.SetSessionState(SessionState{
		SchemaVersion:      SessionStateSchemaV1,
		SelectedInstanceID: "uhost-stale",
		LastIntent:         string(intent.IntentMonitorQuery),
	}, 3)

	e.SetSessionState(SessionState{
		SchemaVersion:      SessionStateSchemaV1,
		SelectedInstanceID: "uhost-fresh",
		LastIntent:         string(intent.IntentResourceInfo),
	}, 4) // strictly higher

	state, ver, _ := e.SessionStateSnapshot()
	assert.Equal(t, "uhost-fresh", state.SelectedInstanceID,
		"higher version must fully overwrite — the in-memory state was stale")
	assert.Equal(t, string(intent.IntentResourceInfo), state.LastIntent)
	assert.Equal(t, 4, ver)
}

// TestSetSessionState_NotHydratedAlwaysFullOverwrite covers the
// single-replica path: ClearSessionState sets hydrated=false, so a
// SetSessionState call after Clear ALWAYS takes the full-overwrite
// branch, regardless of incoming version.
func TestSetSessionState_NotHydratedAlwaysFullOverwrite(t *testing.T) {
	e := newEngineForSessionStateTest(t)
	// Engine starts un-hydrated.
	require.False(t, func() bool { _, _, h := e.SessionStateSnapshot(); return h }())

	// Even with version=0 (smallest), un-hydrated engine must take the
	// full-overwrite branch.
	e.SetSessionState(SessionState{
		SchemaVersion:      SessionStateSchemaV1,
		SelectedInstanceID: "uhost-cold-start",
	}, 0)

	state, ver, hydrated := e.SessionStateSnapshot()
	assert.True(t, hydrated)
	assert.Equal(t, "uhost-cold-start", state.SelectedInstanceID)
	assert.Equal(t, 0, ver)
}

// TestSetSessionState_StrictlyLowerVersion_MustNotRegressVersion closes
// the P2.1 test gap caught by the final review: previously
// TestSetSessionState_VersionAwareMerge_StaleIncomingDoesNotClobber used
// version=5 for both the in-memory and the incoming state, so adding
// `e.sessionStateVersion = version` to the merge branch was a no-op
// assignment and invisible to tests. With a STRICTLY-LOWER incoming
// version, that mutation would silently regress sessionStateVersion,
// breaking the next CAS round-trip.
//
// MUTATION: adding `e.sessionStateVersion = version` to the merge branch
// (engine.go:701) would make this test fail with `ver == 3` instead of
// the expected `ver == 5`.
func TestSetSessionState_StrictlyLowerVersion_MustNotRegressVersion(t *testing.T) {
	e := newEngineForSessionStateTest(t)
	e.SetSessionState(SessionState{
		SchemaVersion:      SessionStateSchemaV1,
		SelectedInstanceID: "uhost-A",
		LastIntent:         string(intent.IntentMonitorQuery),
	}, 5)

	// Stale incoming with STRICTLY lower version (e.g. cross-replica lag).
	e.SetSessionState(SessionState{
		SchemaVersion:      SessionStateSchemaV1,
		SelectedInstanceID: "uhost-stale",
		LastIntent:         string(intent.IntentResourceInfo),
	}, 3)

	state, ver, _ := e.SessionStateSnapshot()
	assert.Equal(t, 5, ver, "version MUST NOT regress when incoming version < in-memory version (CAS round-trip invariant)")
	assert.Equal(t, "uhost-A", state.SelectedInstanceID, "scalars MUST NOT clobber on stale-version merge")
	assert.Equal(t, string(intent.IntentMonitorQuery), state.LastIntent)
}

// TestSetSessionState_FactTie_LocalWinsOnSameProducedAt closes the P2.2
// test gap: previously TestSetSessionState_VersionAwareMerge had no two
// facts sharing the same (Kind, SubjectID, ProducedAtUnix), so swapping
// the merge call to `mergeFactsByProducedAt(state.RecentFacts,
// e.sessionState.RecentFacts)` was invisible.
//
// The merge function's tie semantic is `>` (NOT `>=`), so a tie keeps
// the first-seen value — i.e. local (which is passed first). This must
// be pinned because the cross-replica reconcile rule documented at
// session_state.go is "local wins on tie because it has not yet been
// persisted".
//
// MUTATION: swapping arg order at engine.go:701 to
// `mergeFactsByProducedAt(state.RecentFacts, e.sessionState.RecentFacts)`
// makes this test fail — incoming Payload wins instead of local.
func TestSetSessionState_FactTie_LocalWinsOnSameProducedAt(t *testing.T) {
	e := newEngineForSessionStateTest(t)
	e.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaV1,
		RecentFacts: []ToolFact{
			{Kind: FactKindInstanceState, SubjectID: "uhost-A", ProducedAtUnix: 200,
				Payload: map[string]any{"state": "LOCAL_WINS"}},
		},
	}, 5)

	// Incoming has the same (Kind, SubjectID, ProducedAtUnix) — only the
	// Payload differs.
	e.SetSessionState(SessionState{
		SchemaVersion: SessionStateSchemaV1,
		RecentFacts: []ToolFact{
			{Kind: FactKindInstanceState, SubjectID: "uhost-A", ProducedAtUnix: 200,
				Payload: map[string]any{"state": "INCOMING_LOSES"}},
		},
	}, 5)

	state, _, _ := e.SessionStateSnapshot()
	require.Len(t, state.RecentFacts, 1)
	assert.Equal(t, "LOCAL_WINS", state.RecentFacts[0].Payload["state"],
		"on ProducedAtUnix tie, local must win — incoming arg passed second to mergeFactsByProducedAt loses ties")
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

// TestToolFact_TTLDefaultsByKind verifies the per-kind default TTL constants.
// Adding a new kind requires adding a TTL constant and the case in
// ttlSecondsForKind. Renamed from the original TestToolFact_PayloadKeysPerKind
// to honestly reflect what it tests; payload-key enforcement is in
// TestToolFact_PayloadKeysEnforced below.
func TestToolFact_TTLDefaultsByKind(t *testing.T) {
	assert.Equal(t, factTTLSecondsInstanceState, ttlSecondsForKind(FactKindInstanceState))
	assert.Equal(t, factTTLSecondsMonitorSample, ttlSecondsForKind(FactKindMonitorSample))
	assert.Greater(t, ttlSecondsForKind(FactKindInstanceState), 0,
		"instance_state TTL must be positive — zero would make M3 assembler treat it as expired")
	assert.Greater(t, ttlSecondsForKind(FactKindMonitorSample), 0,
		"monitor_sample TTL must be positive — same reason")
	assert.Equal(t, 0, ttlSecondsForKind("unknown_kind"),
		"unknown kinds must default to TTL=0 so M3 assembler treats them as expired")
}

// TestToolFact_PayloadKeysEnforced is the load-bearing key-set test. It
// asserts that every key emitted by the writer (commit 3) for each kind
// is in the documented set returned by expectedPayloadKeysForKind.
//
// Today (commit 1, no writer wired): we exercise the helper directly with
// canonical inputs. When commit 3 lands, the writer also calls the helper
// so this test continues to enforce the contract on real writer output.
//
// Mutation-test reasoning: adding a stray key like "hallucinated" to a
// canonical payload here makes assertion fail. Adding a key to
// expectedPayloadKeysForKind without using it in production is detected
// by the second assertion (every documented key MUST appear in canonical
// payload — prevents ghost keys from accumulating in the contract).
func TestToolFact_PayloadKeysEnforced(t *testing.T) {
	canonical := map[string]map[string]any{
		FactKindInstanceState: {
			"name":     "gpu-prod",
			"state":    "Running",
			"gpu":      float64(2),
			"gpu_type": "RTX4090",
			"cpu":      float64(16),
			"memory":   float64(64),
			"zone":     "cn-bj-01",
		},
		FactKindMonitorSample: {
			"cpu_usage":         "92.5",
			"memory_usage":      "44.0",
			"gpu_usage":         "88.0",
			"vram_usage":        "70.0",
			"system_disk_usage": "12.0",
			"data_disk_usage":   "8.0",
		},
	}
	for kind, payload := range canonical {
		expected := expectedPayloadKeysForKind(kind)
		require.NotEmpty(t, expected, "kind %q has no expected payload keys", kind)

		// Every key in the canonical payload must be accepted.
		for k := range payload {
			assert.Truef(t, isAcceptedPayloadKey(kind, k),
				"kind %q: canonical key %q must be in expected set", kind, k)
		}
		// Every documented key must appear in the canonical payload.
		// Prevents stale/ghost entries in expectedPayloadKeysForKind.
		for k := range expected {
			_, ok := payload[k]
			assert.Truef(t, ok, "kind %q: documented key %q missing from canonical payload", kind, k)
		}
		// Negative: a stray key must be rejected.
		assert.Falsef(t, isAcceptedPayloadKey(kind, "hallucinated_key"),
			"kind %q: stray key 'hallucinated_key' must NOT be accepted", kind)
	}
}

// TestToolFact_MonitorMultiGPUKeyAccepted covers the monitor_sample
// renderer's multi-GPU disambiguation suffix: "gpu_usage.GPU 1" /
// "gpu_usage.GPU 2" must pass the key check via the dotted-suffix rule.
func TestToolFact_MonitorMultiGPUKeyAccepted(t *testing.T) {
	for _, key := range []string{"gpu_usage.GPU 1", "gpu_usage.GPU 2", "vram_usage.GPU 1"} {
		assert.Truef(t, isAcceptedPayloadKey(FactKindMonitorSample, key),
			"multi-GPU key %q must be accepted via dotted-prefix rule", key)
	}
	// Sanity: a non-prefixed key still fails.
	assert.False(t, isAcceptedPayloadKey(FactKindMonitorSample, "fake_metric.GPU 1"))
	// Sanity: only monitor_sample uses dotted suffixes; instance_state must NOT.
	assert.False(t, isAcceptedPayloadKey(FactKindInstanceState, "name.suffix"))
}

// TestToolFact_NumericPayloadRoundTrip is the load-bearing test for the
// "writers must coerce ints to float64" contract documented on ToolFact.
// Without coercion, json.Unmarshal turns ints into float64 and
// reflect.DeepEqual on the parent struct fails after round-trip.
func TestToolFact_NumericPayloadRoundTrip(t *testing.T) {
	// Pre-coercion: int — would break DeepEqual round-trip.
	raw := map[string]any{"gpu": 2, "cpu": 16, "memory": 64}
	coerced := make(map[string]any, len(raw))
	for k, v := range raw {
		coerced[k] = toFactNumeric(v)
	}
	for _, v := range coerced {
		_, isFloat := v.(float64)
		assert.True(t, isFloat, "toFactNumeric must produce float64 from int input")
	}

	// Round-trip: facts with coerced payload must DeepEqual after JSON.
	fact := ToolFact{
		Kind:           FactKindInstanceState,
		SubjectID:      "uhost-x",
		Payload:        coerced,
		ProducedAtUnix: 100,
		TTLSeconds:     factTTLSecondsInstanceState,
	}
	jsonBytes, err := json.Marshal(fact)
	require.NoError(t, err)
	var got ToolFact
	require.NoError(t, json.Unmarshal(jsonBytes, &got))
	assert.Equal(t, fact, got,
		"coerced numeric payload must reflect.DeepEqual after JSON round-trip")
}

// TestToolFact_NumericCoercionRoundTripMutation demonstrates the failure
// mode: WITHOUT coercion, the int-typed payload fails DeepEqual. This
// test pins the contract documented on ToolFact: callers who skip
// toFactNumeric will silently produce non-round-trippable facts.
func TestToolFact_NumericCoercionRoundTripMutation(t *testing.T) {
	fact := ToolFact{
		Kind:           FactKindInstanceState,
		SubjectID:      "uhost-x",
		Payload:        map[string]any{"gpu": 2}, // int — violates contract
		ProducedAtUnix: 100,
	}
	jsonBytes, err := json.Marshal(fact)
	require.NoError(t, err)
	var got ToolFact
	require.NoError(t, json.Unmarshal(jsonBytes, &got))
	assert.NotEqual(t, fact, got,
		"WITHOUT toFactNumeric, int payload fails DeepEqual after round-trip — this is exactly what writers must avoid")
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

// TestMergeFactsByProducedAt_PayloadIsolated exercises the
// shallow-clone-on-store contract: mutating a Payload key on the merge
// output must NOT affect the corresponding input fact's Payload, even
// though both came from the same writer's map. This is required because
// the engine's in-memory facts are one of the merge inputs; without
// payload isolation, M3 ContextAssembler mutating output Payloads would
// silently corrupt the engine state.
func TestMergeFactsByProducedAt_PayloadIsolated(t *testing.T) {
	local := []ToolFact{
		{Kind: FactKindInstanceState, SubjectID: "uhost-A", ProducedAtUnix: 200,
			Payload: map[string]any{"state": "Running"}},
	}
	out := mergeFactsByProducedAt(local, nil)
	require.Len(t, out, 1)

	out[0].Payload["state"] = "Stopped" // mutate the output

	assert.Equal(t, "Running", local[0].Payload["state"],
		"mutating merge output Payload must NOT bleed back to input — shallow-clone-on-store guards against alias")
}

// TestAppendFactToSlice_PayloadIsolated mirrors the previous test for
// the append helper. Same contract, different code path.
func TestAppendFactToSlice_PayloadIsolated(t *testing.T) {
	original := map[string]any{"state": "Running"}
	out := appendFactToSlice(nil, ToolFact{
		Kind: FactKindInstanceState, SubjectID: "uhost-A", ProducedAtUnix: 100,
		Payload: original,
	})
	require.Len(t, out, 1)

	// Mutate the writer's map after store — must not affect stored fact.
	original["state"] = "Stopped"
	assert.Equal(t, "Running", out[0].Payload["state"],
		"mutating original Payload after append MUST NOT change the stored fact (shallow-clone-on-store)")

	// Mutate the stored fact's Payload — must not affect the writer's map.
	out[0].Payload["state"] = "Aborted"
	assert.Equal(t, "Stopped", original["state"],
		"mutating stored Payload must NOT bleed back to original — bidirectional isolation")
}
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
