package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/compshare-agent/internal/knowledge"
)

// SessionStateSchemaCurrent is the persisted JSON schema version this binary
// writes, as "<major>.<minor>". A row from an earlier version decodes through
// the same struct: retired fields are ignored and disappear on the next write,
// and the server-owned continuation cursors it may carry are dropped because
// an earlier schema bound them to a different contract (SetSessionState). A row
// from a later version is left untouched for the binary that wrote it.
const SessionStateSchemaCurrent = "11.0"

// ErrUnknownSessionStateSchema is returned by ParsePersistedContext when a
// row looks like an agent envelope (top-level object with an
// agent_session_state.schema_version string) but the version is newer than
// SessionStateSchemaCurrent or not a version at all. Callers (handleChat) MUST
// treat this like a parse failure: continue the chat turn but skip persistence
// so the row is left untouched for a binary version that recognizes it. See
// ParsePersistedContext for the compatibility rationale.
var ErrUnknownSessionStateSchema = errors.New("engine: unknown SessionState schema_version")

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
// Older rows can contain retired semantic fields. Normal JSON decoding ignores
// them, and a later write omits them without a migration.
type SessionState struct {
	SchemaVersion             string                           `json:"schema_version"`
	SelectedInstanceID        string                           `json:"selected_instance_id,omitempty"`
	SelectedInstanceName      string                           `json:"selected_instance_name,omitempty"`
	SelectedInstanceSource    string                           `json:"selected_instance_source,omitempty"`
	SelectedInstanceAtUnix    int64                            `json:"selected_instance_at_unix,omitempty"`
	SelectedInstanceFreshness string                           `json:"selected_instance_freshness,omitempty"`
	VerifiedEvidence          []VerifiedEvidenceTurn           `json:"verified_knowledge,omitempty"`
	PersistedInstanceOpsJobs  []PersistedInstanceOpsJob        `json:"persisted_instance_ops_jobs,omitempty"`
	PersistedInstanceOpsAgent PersistedInstanceOpsAgentSession `json:"persisted_instance_ops_agent,omitzero"`
}

// PersistedInstanceOpsJob is a durable observation cursor for a
// reviewed background job in a tenant guest. Purpose is a bounded human
// description; it is not executable. Command text and command output are
// intentionally absent from this type and therefore cannot enter SessionState.
//
// Jobs are keyed by instance and job ID; a terminal observation removes only
// that job. An active service and a package installation can therefore coexist.
type PersistedInstanceOpsJob struct {
	InstanceID string `json:"instance_id,omitempty"`
	JobID      string `json:"job_id,omitempty"`
	State      string `json:"state,omitempty"`
	Purpose    string `json:"purpose,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}

// IsZero lets encoding/json's omitzero omit the inactive job slot.
func (j PersistedInstanceOpsJob) IsZero() bool {
	return j == (PersistedInstanceOpsJob{})
}

// PersistedInstanceOpsAgentSession points at an SDK-owned local transcript without copying any
// transcript entry into sessions.context. A session is resumed only for the same target and current
// contract/model; each attempt forks away from this committed cursor. Wall-clock time alone does not
// break continuity. If the SDK-local record is gone, the harness starts fresh from the complete outer
// conversation snapshot.
type PersistedInstanceOpsAgentSession struct {
	InstanceID         string `json:"instance_id,omitempty"`
	SessionID          string `json:"session_id,omitempty"`
	WorkdirID          string `json:"workdir_id,omitempty"`
	Contract           string `json:"contract,omitempty"`
	Model              string `json:"model,omitempty"`
	ConversationAnchor string `json:"conversation_anchor,omitempty"`
	UpdatedAt          string `json:"updated_at,omitempty"`
}

func (s PersistedInstanceOpsAgentSession) IsZero() bool {
	return s == (PersistedInstanceOpsAgentSession{})
}

// VerifiedEvidenceTurn is compact, persisted provenance for an answer that
// already passed the evidence verifier. It is a reference-only ledger:
// it can support later read-only prose, never authorize writes or satisfy a
// real-time state query.
// The JSON key remains verified_knowledge for stored-session compatibility.
type VerifiedEvidenceTurn struct {
	Question       string                   `json:"question,omitempty"`
	Evidence       knowledge.EvidenceLedger `json:"evidence"`
	VerifiedAtUnix int64                    `json:"verified_at_unix,omitempty"`
}

const (
	// SelectedInstanceSourceObserved marks a current-instance referent recorded
	// from a tool observation (a read saw it). It is understanding-only — it helps
	// resolve who "它" is — and is NEVER a selection proof for a write target.
	SelectedInstanceSourceObserved = "observed"
	// SelectedInstanceSourceUser records a confirmed platform operation's target
	// or the sole instance created by a confirmed operation. Existing sessions may
	// also contain earlier explicit selections. It is referential context, not
	// authority for a later operation; client-provided version-0 Context cannot
	// mint this source.
	SelectedInstanceSourceUser = "user_selected"
)

// MarshalJSON ensures SchemaVersion is always present on the wire even if
// a caller zeroed the struct.
func (s SessionState) MarshalJSON() ([]byte, error) {
	if s.SchemaVersion == "" {
		s.SchemaVersion = SessionStateSchemaCurrent
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
//  1. NULL / empty / whitespace-only:  first-time hydrate. Returns zero
//     PersistedContext with no error.
//  2. Known envelope:                  top-level object with
//     agent_session_state.schema_version
//     at or below SessionStateSchemaCurrent.
//     Decoded as the real envelope.
//  3. Unknown envelope version:        top-level object with
//     agent_session_state.schema_version
//     string, but newer than this binary
//     or not a version. Returns
//     ErrUnknownSessionStateSchema so
//     the caller skips persistence and
//     the row is left untouched for a
//     newer binary to read.
//  4. Legacy / anything else:          object without agent_session_state,
//     object whose agent_session_state
//     is not an object or whose
//     schema_version is missing/non-string,
//     array, string, number, bool, etc.
//     Treated as opaque client_context,
//     preserved verbatim, and upgraded
//     to a known envelope on the next
//     successful chat-turn persist.
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
			AgentSessionState: SessionState{SchemaVersion: SessionStateSchemaCurrent},
		}, nil
	}
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return PersistedContext{}, err
	}
	switch classifyEnvelope(probe) {
	case envelopeKindKnown:
		// Decode the two ownership domains independently. A type error inside
		// agent_session_state must not hide a valid client_context from the
		// caller that will self-heal only the agent-owned half.
		var wire struct {
			AgentSessionState json.RawMessage `json:"agent_session_state"`
			ClientContext     json.RawMessage `json:"client_context"`
		}
		if err := json.Unmarshal(raw, &wire); err != nil {
			return PersistedContext{}, err
		}
		pc := PersistedContext{ClientContext: append(json.RawMessage(nil), wire.ClientContext...)}
		if err := json.Unmarshal(wire.AgentSessionState, &pc.AgentSessionState); err != nil {
			return pc, err
		}
		if pc.AgentSessionState.SchemaVersion == "" {
			pc.AgentSessionState.SchemaVersion = SessionStateSchemaCurrent
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
			AgentSessionState: SessionState{SchemaVersion: SessionStateSchemaCurrent},
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
	if schemaVersionReadable(ver) {
		return envelopeKindKnown
	}
	return envelopeKindUnknownVersion
}

// schemaVersionReadable reports whether ver is a "<major>.<minor>" version this
// binary may read: 1.0 up to and including SessionStateSchemaCurrent. Anything
// newer, or not of that shape, belongs to another binary.
func schemaVersionReadable(ver string) bool {
	major, minor, ok := parseSchemaVersion(ver)
	if !ok || major < 1 {
		return false
	}
	currentMajor, currentMinor, _ := parseSchemaVersion(SessionStateSchemaCurrent)
	return major < currentMajor || (major == currentMajor && minor <= currentMinor)
}

func parseSchemaVersion(ver string) (major, minor int, ok bool) {
	majorText, minorText, found := strings.Cut(ver, ".")
	if !found {
		return 0, 0, false
	}
	parse := func(text string) (int, bool) {
		if text == "" || strings.TrimLeft(text, "0123456789") != "" {
			return 0, false
		}
		n, err := strconv.Atoi(text)
		return n, err == nil
	}
	if major, ok = parse(majorText); !ok {
		return 0, 0, false
	}
	if minor, ok = parse(minorText); !ok {
		return 0, 0, false
	}
	return major, minor, true
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
