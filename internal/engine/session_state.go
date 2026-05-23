package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
// M1 ships with the minimum viable set. ToolFacts / known_constraints /
// pending_action are deferred to M2+.
//
// LastIntent vocabulary contract: when non-empty, the value MUST be
// exactly string(intent.X) for some X in intent.RuntimeIntents() — e.g.
// "monitor_query", "resource_info", "gpu_specs_query". Short aliases like
// "monitor" / "resource" are not legal. The string typing here is
// intentional to keep the engine package from reverse-depending on the
// intent package; the test in session_state_test.go enforces the
// vocabulary contract via test-side import of internal/intent.
type SessionState struct {
	SchemaVersion        string `json:"schema_version"`
	SelectedInstanceID   string `json:"selected_instance_id,omitempty"`
	SelectedInstanceName string `json:"selected_instance_name,omitempty"`
	LastIntent           string `json:"last_intent,omitempty"`
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
