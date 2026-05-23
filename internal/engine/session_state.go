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
// TestToolFact_PayloadKeysPerKind_Enforced via marshal/round-trip.
//
// Numeric types in Payload: writers MUST coerce ints to float64 (or
// keep strings) at write time, because json.Unmarshal turns every JSON
// number into float64 and downstream readers must see stable types.
// Direct `int` storage in Payload breaks reflect.DeepEqual after
// round-trip; the writer in commit 3 calls toFactNumeric to enforce.
//
// Payload empty-map vs nil: omitempty makes both serialize to no key on
// the wire, and unmarshal always restores nil. Readers MUST use
// `len(payload) > 0`, NOT `payload != nil`.
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
// kind this binary doesn't recognize. This is consulted at WRITE time by
// the in-engine writer (commit 3); M3's read-side defensive fallback may
// also consult it when ToolFact.TTLSeconds is zero (omitempty-stripped).
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

// expectedPayloadKeysForKind returns the documented payload-key set per
// fact kind. Used by writers (commit 3) to validate output and by tests
// to enforce the contract. Adding a new key here is a deliberate API
// change — the test TestToolFact_PayloadKeysEnforced asserts that every
// payload key emitted by the writer is in this set.
//
// monitor_sample multi-GPU keys: the renderer at internal/intent/envelope.go
// produces gpu_usage.GPU 1 / .GPU 2 / .GPU 3 / .GPU 4 for multi-GPU hosts.
// We treat the dotted-suffix keys as the same logical "gpu_usage" entry;
// the test allows any key with the documented prefix.
func expectedPayloadKeysForKind(kind string) map[string]struct{} {
	switch kind {
	case FactKindInstanceState:
		return map[string]struct{}{
			"name":     {},
			"state":    {},
			"gpu":      {},
			"gpu_type": {},
			"cpu":      {},
			"memory":   {},
			"zone":     {},
		}
	case FactKindMonitorSample:
		return map[string]struct{}{
			"cpu_usage":         {},
			"memory_usage":      {},
			"gpu_usage":         {},
			"vram_usage":        {},
			"system_disk_usage": {},
			"data_disk_usage":   {},
		}
	default:
		return nil
	}
}

// isAcceptedPayloadKey returns true when key is in expectedPayloadKeysForKind
// for the kind. For monitor_sample only, dotted-suffix keys ("gpu_usage.GPU 1")
// are also accepted when the prefix matches an expected base key — this
// handles the renderer's multi-GPU disambiguation (internal/intent/envelope.go).
// Other kinds' keys must match exactly.
func isAcceptedPayloadKey(kind, key string) bool {
	expected := expectedPayloadKeysForKind(kind)
	if _, ok := expected[key]; ok {
		return true
	}
	if kind != FactKindMonitorSample {
		return false
	}
	if dot := indexByte(key, '.'); dot > 0 {
		base := key[:dot]
		if _, ok := expected[base]; ok {
			return true
		}
	}
	return false
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// toFactNumeric coerces an integer scalar to float64 so JSON round-trip
// is reflect.DeepEqual-stable. Strings, floats, bools pass through. Any
// other type returns the empty string (writers should not store rich
// types in Payload anyway).
//
// Why this exists: json.Unmarshal turns every JSON number into float64.
// A writer storing `int(2)` in Payload produces `{"gpu":2}` on the wire
// but unmarshal restores `float64(2)`, breaking reflect.DeepEqual on
// the parent ToolFact. M3 ContextAssembler reads Payload values via
// type-switch; coercing to float64 at write time keeps the reader
// type-stable.
func toFactNumeric(v any) any {
	switch x := v.(type) {
	case int:
		return float64(x)
	case int8:
		return float64(x)
	case int16:
		return float64(x)
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	case uint:
		return float64(x)
	case uint8:
		return float64(x)
	case uint16:
		return float64(x)
	case uint32:
		return float64(x)
	case uint64:
		return float64(x)
	case float32:
		return float64(x)
	case float64:
		return x
	case string:
		return x
	case bool:
		return x
	default:
		return ""
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

// copyFactPayload returns a shallow-cloned map[string]any. Used by
// appendFactToSlice and mergeFactsByProducedAt to break the alias between
// input fact Payload and stored fact Payload — without this, mutating
// the merged-output Payload would silently mutate the engine's
// in-memory facts (and vice-versa).
//
// Values are not deep-cloned because Payload contract per ToolFact docstring
// is "flat scalar map" — readers must not store nested structures.
func copyFactPayload(payload map[string]any) map[string]any {
	if payload == nil {
		return nil
	}
	out := make(map[string]any, len(payload))
	for k, v := range payload {
		out[k] = v
	}
	return out
}

// cloneFact returns a fact with a fresh-cloned Payload map. Used at
// every store/insert boundary in append/merge helpers so callers cannot
// accidentally mutate stored Payloads via aliased map references.
func cloneFact(f ToolFact) ToolFact {
	f.Payload = copyFactPayload(f.Payload)
	return f
}

// appendFactToSlice inserts fact into facts, deduping by (Kind, SubjectID).
// If a fact with the same key already exists, the newer ProducedAtUnix
// wins; ties go to the new fact (caller intent on overwrite, ">=" semantics).
// Output is sorted ProducedAtUnix descending and capped at maxRecentFacts
// (oldest dropped).
//
// Pure on inputs: input slice header is not mutated; input fact Payload
// is shallow-cloned before storing, so subsequent mutations to the
// caller's map do NOT affect the stored fact, and vice-versa.
func appendFactToSlice(facts []ToolFact, fact ToolFact) []ToolFact {
	stored := cloneFact(fact)
	out := make([]ToolFact, 0, len(facts)+1)
	replaced := false
	for _, f := range facts {
		if f.Kind == stored.Kind && f.SubjectID == stored.SubjectID {
			if replaced {
				continue
			}
			if stored.ProducedAtUnix >= f.ProducedAtUnix {
				out = append(out, stored)
			} else {
				out = append(out, cloneFact(f))
			}
			replaced = true
			continue
		}
		out = append(out, cloneFact(f))
	}
	if !replaced {
		out = append(out, stored)
	}
	sortFactsByProducedAtDesc(out)
	if len(out) > maxRecentFacts {
		out = out[:maxRecentFacts]
	}
	return out
}

// mergeFactsByProducedAt merges two fact lists, deduping by (Kind,
// SubjectID), keeping the higher ProducedAtUnix per key. Ties keep the
// existing entry (local wins on tie, ">" semantics). Output is sorted
// ProducedAtUnix descending and capped at maxRecentFacts. Used by
// SetSessionState's version-aware merge path (see engine.go).
//
// Pure on inputs: neither input slice header is mutated; per-fact Payload
// maps are shallow-cloned before storing, so subsequent mutations to
// caller maps do NOT affect stored facts.
//
// Tie-break asymmetry vs appendFactToSlice (`>=` there, `>` here) is
// intentional: append is the in-engine write path where the writer
// always wants its newest fact to take effect; merge is the cross-replica
// reconcile path where local in-memory state is authoritative on ties
// because it has not yet been persisted.
func mergeFactsByProducedAt(local, incoming []ToolFact) []ToolFact {
	out := make([]ToolFact, 0, len(local)+len(incoming))
	seen := make(map[string]int, len(local)+len(incoming))
	insertOrReplace := func(f ToolFact) {
		key := f.Kind + "\x00" + f.SubjectID
		if idx, ok := seen[key]; ok {
			if f.ProducedAtUnix > out[idx].ProducedAtUnix {
				out[idx] = cloneFact(f)
			}
			return
		}
		seen[key] = len(out)
		out = append(out, cloneFact(f))
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
