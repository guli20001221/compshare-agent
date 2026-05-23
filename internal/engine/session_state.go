package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// SessionStateSchemaV1 is the persisted JSON schema version for SessionState.
// Bump on any breaking shape change. Old records load with their original
// SchemaVersion and are migrated forward by future loaders.
const SessionStateSchemaV1 = "1.0"

// ErrUnknownSessionStateSchema is returned by ParsePersistedContext when a
// row looks like an agent envelope (top-level object with an
// agent_session_state.schema_version string) but the version is not in
// knownSessionStateSchemaVersions. Callers (handleChat) MUST treat this
// like a parse failure: continue the chat turn but skip persistence so
// the row is left untouched for the binary version that does recognize
// it. See ParsePersistedContext docstring for the rollout rationale.
var ErrUnknownSessionStateSchema = errors.New("engine: unknown SessionState schema_version")

// knownSessionStateSchemaVersions enumerates every schema_version string
// this binary recognizes as an agent-owned envelope. Probing for any of
// these inside agent_session_state.schema_version is what distinguishes a
// true envelope from a legacy client blob that happens to carry the
// same top-level key.
//
// When bumping SessionStateSchemaV1 to a new version, append the new
// constant here. Removing an entry is a breaking change to the on-wire
// envelope detection — be very explicit if you do it.
var knownSessionStateSchemaVersions = map[string]struct{}{
	SessionStateSchemaV1: {},
}

// SessionState is the per-session, JSON-serializable, multi-replica-safe
// snapshot of agent-level dialog state. It MUST be fully round-trip-able:
//
//	state → JSON → SetSessionState → SessionStateSnapshot → JSON
//
// must be byte-equal (or semantically equal after canonical re-marshal).
//
// All fields are exported, JSON-tagged, and contain no pointers, no cache
// handles, and no unexported implicit state. Adding a field requires:
//
//	(1) JSON tag with omitempty for backwards compat, and
//	(2) extending the round-trip test in session_state_test.go.
//
// M1 shipped with 4 scalar fields. M2 added RecentFacts. known_constraints /
// pending_action remain deferred.
//
// LastIntent vocabulary contract: when non-empty, the value MUST be
// exactly string(intent.X) for some X in intent.RuntimeIntents() — e.g.
// "monitor_query", "resource_info", "gpu_specs_query". Short aliases like
// "monitor" / "resource" are not legal. The string typing here is
// intentional to keep the engine package from reverse-depending on the
// intent package; the test in session_state_test.go enforces the
// vocabulary contract via test-side import of internal/intent.
type SessionState struct {
	SchemaVersion        string     `json:"schema_version"`
	SelectedInstanceID   string     `json:"selected_instance_id,omitempty"`
	SelectedInstanceName string     `json:"selected_instance_name,omitempty"`
	LastIntent           string     `json:"last_intent,omitempty"`
	RecentFacts          []ToolFact `json:"recent_facts,omitempty"`
}

// ToolFact is one piece of evidence accumulated from a successful tool
// call. M2 introduces this; M3 ContextAssembler will be the first reader.
//
// Multi-replica preservation contract (see project_multi_replica_interfaces
// memory §2): each fact carries ProducedAtUnix; conflict resolution between
// replicas picks the higher value. The (Kind, SubjectID) pair is the dedupe
// key — only one live fact per pair, and overwrite keeps the newer
// ProducedAtUnix.
//
// Round-trip contract (inherited from SessionState §1): no pointers, no
// unexported fields, no time.Time (use unix int64). Payload is a flat
// scalar map; concrete keys per Kind are enforced by
// TestToolFact_PayloadKeysPerKind.
//
// TTL: a zero TTLSeconds means "use kind default at read time" — the
// writer always populates TTLSeconds with ttlSecondsForKind(Kind) for
// known kinds, so zero on read indicates an unknown/future kind.
type ToolFact struct {
	Kind           string         `json:"kind"`
	SubjectID      string         `json:"subject_id"`
	Payload        map[string]any `json:"payload,omitempty"`
	ProducedAtTurn int            `json:"produced_at_turn"`
	ProducedAtUnix int64          `json:"produced_at_unix"`
	TTLSeconds     int            `json:"ttl_seconds,omitempty"`
}

// ToolFact kind constants. New kinds must be added here, in
// ttlSecondsForKind, and in the round-trip test's expected-keys map.
const (
	FactKindInstanceState = "instance_state"
	FactKindMonitorSample = "monitor_sample"
)

// Per-kind TTL constants. Aligned with [[project-context-first-roadmap]]
// rule 1: instance_state 15-30s, monitor 10-30s, pricing/stock short. We
// pick the upper end of the range — facts are descriptive ("刚才那个 CPU
// 高是什么意思"), and re-query for "现在还高吗" is enforced by the existing
// force-recall mechanism at engine.go:3438-3444.
const (
	factTTLSecondsInstanceState = 30
	factTTLSecondsMonitorSample = 30
)

// maxRecentFacts caps RecentFacts slice length to bound persist payload
// size. Empirical: a 7-instance account producing two fact kinds per
// instance maxes at 14, so 16 leaves headroom without unbounded growth.
const maxRecentFacts = 16

// ttlSecondsForKind returns the default TTL for known fact kinds. Unknown
// kinds return 0, which M3 ContextAssembler must treat as "expired" — a
// forward-rollout safety net for facts written by a future binary on a
// kind this binary doesn't recognize.
func ttlSecondsForKind(kind string) int {
	switch kind {
	case FactKindInstanceState:
		return factTTLSecondsInstanceState
	case FactKindMonitorSample:
		return factTTLSecondsMonitorSample
	default:
		return 0
	}
}

// MarshalJSON ensures SchemaVersion is always present on the wire even if
// a caller zeroed the struct.
func (s SessionState) MarshalJSON() ([]byte, error) {
	if s.SchemaVersion == "" {
		s.SchemaVersion = SessionStateSchemaV1
	}
	type alias SessionState
	return json.Marshal(alias(s))
}

// PersistedContext is the on-wire shape stored in sessions.context. It
// exists to preserve the public CreateCSAgentSession Context API param —
// clients may write an arbitrary JSON blob via that param, and the agent
// must not silently overwrite it on chat-turn persistence.
//
// Four cases ParsePersistedContext handles:
//
//	1. NULL / empty / whitespace-only:  first-time hydrate. Returns zero
//	                                    PersistedContext with no error.
//	2. Known envelope:                  top-level object with
//	                                    agent_session_state.schema_version
//	                                    in knownSessionStateSchemaVersions.
//	                                    Decoded as the real envelope.
//	3. Unknown envelope version:        top-level object with
//	                                    agent_session_state.schema_version
//	                                    string, but the version is not
//	                                    recognized by this binary. Returns
//	                                    ErrUnknownSessionStateSchema so
//	                                    the caller skips persistence and
//	                                    the row is left untouched for a
//	                                    newer binary to read.
//	4. Legacy / anything else:          object without agent_session_state,
//	                                    object whose agent_session_state
//	                                    is not an object or whose
//	                                    schema_version is missing/non-string,
//	                                    array, string, number, bool, etc.
//	                                    Treated as opaque client_context,
//	                                    preserved verbatim, and upgraded
//	                                    to a known envelope on the next
//	                                    successful chat-turn persist.
//
// AgentSessionState is what Engine sees via SetSessionState; ClientContext
// is preserved opaquely by the http layer across read/write.
type PersistedContext struct {
	AgentSessionState SessionState    `json:"agent_session_state"`
	ClientContext     json.RawMessage `json:"client_context,omitempty"`
}

// ParsePersistedContext decodes the sessions.context column value. See
// PersistedContext docstring for the four cases. On malformed JSON it
// returns (zero, err) — callers MUST NOT persist after any non-nil error
// (parse failure or unknown schema), or a transient/forward-rollout
// condition becomes permanent state loss.
func ParsePersistedContext(raw json.RawMessage) (PersistedContext, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte(`null`)) {
		return PersistedContext{
			AgentSessionState: SessionState{SchemaVersion: SessionStateSchemaV1},
		}, nil
	}
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return PersistedContext{}, err
	}
	switch classifyEnvelope(probe) {
	case envelopeKindKnown:
		var pc PersistedContext
		if err := json.Unmarshal(raw, &pc); err != nil {
			return PersistedContext{}, err
		}
		if pc.AgentSessionState.SchemaVersion == "" {
			pc.AgentSessionState.SchemaVersion = SessionStateSchemaV1
		}
		return pc, nil
	case envelopeKindUnknownVersion:
		ver, _ := extractAgentSchemaVersion(probe)
		return PersistedContext{}, fmt.Errorf("%w: %q", ErrUnknownSessionStateSchema, ver)
	default:
		// Legacy: opaque client_context, preserved verbatim. Will be
		// upgraded to a known envelope on the next successful persist.
		legacy := make(json.RawMessage, len(raw))
		copy(legacy, raw)
		return PersistedContext{
			AgentSessionState: SessionState{SchemaVersion: SessionStateSchemaV1},
			ClientContext:     legacy,
		}, nil
	}
}

// envelopeKind classifies the decoded top-level JSON value. See
// PersistedContext docstring for the four cases.
type envelopeKind int

const (
	envelopeKindLegacy envelopeKind = iota
	envelopeKindKnown
	envelopeKindUnknownVersion
)

// classifyEnvelope inspects the decoded JSON to decide whether to parse
// the row as an envelope, refuse it as a forward-rollout unknown version,
// or treat it as opaque legacy client_context.
func classifyEnvelope(probe any) envelopeKind {
	ver, ok := extractAgentSchemaVersion(probe)
	if !ok {
		return envelopeKindLegacy
	}
	if _, known := knownSessionStateSchemaVersions[ver]; known {
		return envelopeKindKnown
	}
	return envelopeKindUnknownVersion
}

// extractAgentSchemaVersion returns (version, true) only when probe is an
// object whose agent_session_state is an object containing a string-typed
// schema_version. All other shapes return ("", false).
func extractAgentSchemaVersion(probe any) (string, bool) {
	top, ok := probe.(map[string]interface{})
	if !ok {
		return "", false
	}
	inner, ok := top["agent_session_state"].(map[string]interface{})
	if !ok {
		return "", false
	}
	ver, ok := inner["schema_version"].(string)
	if !ok {
		return "", false
	}
	return ver, true
}

// appendFactToSlice inserts fact into facts, deduping by (Kind, SubjectID).
// If a fact with the same key already exists, the newer ProducedAtUnix
// wins; ties go to the new fact (caller intent on overwrite). Output is
// sorted ProducedAtUnix descending and capped at maxRecentFacts (oldest
// dropped).
//
// Pure function. Caller must not assume the input slice is unmodified —
// the implementation prefers in-place compaction where safe — but the
// returned slice is the canonical result; ignore the input slice after
// calling.
func appendFactToSlice(facts []ToolFact, fact ToolFact) []ToolFact {
	out := make([]ToolFact, 0, len(facts)+1)
	replaced := false
	for _, f := range facts {
		if f.Kind == fact.Kind && f.SubjectID == fact.SubjectID {
			if replaced {
				continue
			}
			if fact.ProducedAtUnix >= f.ProducedAtUnix {
				out = append(out, fact)
			} else {
				out = append(out, f)
			}
			replaced = true
			continue
		}
		out = append(out, f)
	}
	if !replaced {
		out = append(out, fact)
	}
	sortFactsByProducedAtDesc(out)
	if len(out) > maxRecentFacts {
		out = out[:maxRecentFacts]
	}
	return out
}

// mergeFactsByProducedAt merges two fact lists, deduping by (Kind,
// SubjectID), keeping the higher ProducedAtUnix per key. Output is sorted
// ProducedAtUnix descending and capped at maxRecentFacts. Used by
// SetSessionState's version-aware merge path (see engine.go).
//
// Pure function. Neither input slice is mutated. The output is a fresh
// slice — safe to assign over either input.
func mergeFactsByProducedAt(local, incoming []ToolFact) []ToolFact {
	out := make([]ToolFact, 0, len(local)+len(incoming))
	seen := make(map[string]int, len(local)+len(incoming))
	insertOrReplace := func(f ToolFact) {
		key := f.Kind + "\x00" + f.SubjectID
		if idx, ok := seen[key]; ok {
			if f.ProducedAtUnix > out[idx].ProducedAtUnix {
				out[idx] = f
			}
			return
		}
		seen[key] = len(out)
		out = append(out, f)
	}
	for _, f := range local {
		insertOrReplace(f)
	}
	for _, f := range incoming {
		insertOrReplace(f)
	}
	sortFactsByProducedAtDesc(out)
	if len(out) > maxRecentFacts {
		out = out[:maxRecentFacts]
	}
	return out
}

// sortFactsByProducedAtDesc sorts in place by ProducedAtUnix descending.
// Stable across ties: facts with the same ProducedAtUnix keep relative
// order, so callers can rely on insertion-order tiebreak when timestamps
// collide within a single turn.
func sortFactsByProducedAtDesc(facts []ToolFact) {
	sort.SliceStable(facts, func(i, j int) bool {
		return facts[i].ProducedAtUnix > facts[j].ProducedAtUnix
	})
}
