package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/compshare-agent/internal/knowledge"
)

// SessionStateSchemaV1 is the first persisted JSON schema version for SessionState.
const SessionStateSchemaV1 = "1.0"

// SessionStateSchemaV2 historically added a persisted context frame. The field is
// now retired, but V2 rows remain readable so a later write can drop that unused
// semantic sidecar without a migration.
const SessionStateSchemaV2 = "2.0"

// SessionStateSchemaV3 historically extended the retired context frame. It is
// retained only as a recognized on-wire version for existing sessions.
const SessionStateSchemaV3 = "3.0"

// SessionStateSchemaV4 records the trust source for selected instances. Its old
// workflow-slot fields are ignored; selection provenance remains live.
const SessionStateSchemaV4 = "4.0"

// SessionStateSchemaV5 added fields that are now retired plus explicit selection
// freshness. V5 rows remain readable and unknown fields disappear on rewrite.
const SessionStateSchemaV5 = "5.0"

// SessionStateSchemaV6 persists bounded evidence from answers that passed the
// semantic knowledge verifier. It lets a cold/restarted agent validate a short
// follow-up against the same source instead of trusting arbitrary assistant text
// or forcing another retrieval.
const SessionStateSchemaV6 = "6.0"

// SessionStateSchemaV7 retired older summary fields. Those fields are
// deliberately ignored when old rows are decoded.
const SessionStateSchemaV7 = "7.0"

// SessionStateSchemaV8 persists one opaque in-instance background-job handle.
// It deliberately stores no command or output: the handle is sufficient to poll
// the reviewed guest job after an Engine rebuild, while the original operation
// remains outside conversation/session persistence.
const SessionStateSchemaV8 = "8.0"

const SessionStateSchemaCurrent = SessionStateSchemaV8

// ErrUnknownSessionStateSchema is returned by ParsePersistedContext when a
// row looks like an agent envelope (top-level object with an
// agent_session_state.schema_version string) but the version is not in
// knownSessionStateSchemaVersions. Callers (handleChat) MUST treat this
// like a parse failure: continue the chat turn but skip persistence so
// the row is left untouched for a binary version that recognizes it. See
// ParsePersistedContext for the compatibility rationale.
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
	SessionStateSchemaV2: {},
	SessionStateSchemaV3: {},
	SessionStateSchemaV4: {},
	SessionStateSchemaV5: {},
	SessionStateSchemaV6: {},
	SessionStateSchemaV7: {},
	SessionStateSchemaV8: {},
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
// Older rows can contain retired semantic fields. Normal JSON decoding ignores
// them, and a later write omits them without a migration.
type SessionState struct {
	SchemaVersion                  string                  `json:"schema_version"`
	SelectedInstanceID             string                  `json:"selected_instance_id,omitempty"`
	SelectedInstanceName           string                  `json:"selected_instance_name,omitempty"`
	SelectedInstanceSource         string                  `json:"selected_instance_source,omitempty"`
	SelectedInstanceAtUnix         int64                   `json:"selected_instance_at_unix,omitempty"`
	SelectedInstanceFreshness      string                  `json:"selected_instance_freshness,omitempty"`
	PendingSelectionKind           string                  `json:"pending_selection_kind,omitempty"`
	PendingSelectionProducedAtUnix int64                   `json:"pending_selection_produced_at_unix,omitempty"`
	PendingSelectionTTLSeconds     int                     `json:"pending_selection_ttl_seconds,omitempty"`
	PendingSelectionItems          []PendingSelectionItem  `json:"pending_selection_items,omitempty"`
	VerifiedEvidence               []VerifiedEvidenceTurn  `json:"verified_knowledge,omitempty"`
	PersistedInstanceOpsJob        PersistedInstanceOpsJob `json:"persisted_instance_ops_job,omitzero"`
}

// PersistedInstanceOpsJob is the single durable observation cursor for a
// reviewed background job in a tenant guest. Purpose is a redacted, bounded
// human description; it is not executable. Command text and command output are
// intentionally absent from this type and therefore cannot enter SessionState.
//
// One slot is intentional. While it contains an active job, an event for a
// different instance/job cannot replace it; terminal state for the matching job
// clears the slot.
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
	// SelectedInstanceSourceObserved marks a current-instance binding recorded
	// from a tool observation (a read saw it). It is understanding-only — it helps
	// resolve who "它" is — and is NEVER a selection proof for a write target.
	SelectedInstanceSourceObserved = "observed"
	// SelectedInstanceSourceUser marks a binding the user genuinely established
	// this session — an explicit id they typed, a pick from a shown selection
	// card, or the sole result of a create they confirmed. While fresh, only this
	// (and a shown-card pick / the account's sole
	// instance) is a SelectionProof; after expiry it remains provenance for a new
	// target-specific card but never authorizes entry or a write on its own.
	SelectedInstanceSourceUser = "user_selected"
)

// PendingSelectionItem is one option from the most recent structured candidate
// list shown or implied by the agent. It is intentionally a compact subset of
// entity.InstanceSnapshot so it can be safely persisted in session context.
type PendingSelectionItem struct {
	Index      int    `json:"index,omitempty"`
	ID         string `json:"id,omitempty"`
	Name       string `json:"name,omitempty"`
	State      string `json:"state,omitempty"`
	GPU        int    `json:"gpu,omitempty"`
	GpuType    string `json:"gpu_type,omitempty"`
	CPU        int    `json:"cpu,omitempty"`
	Memory     int    `json:"memory,omitempty"`
	Zone       string `json:"zone,omitempty"`
	Region     string `json:"region,omitempty"`
	ChargeType string `json:"charge_type,omitempty"`
}

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
//     in knownSessionStateSchemaVersions.
//     Decoded as the real envelope.
//  3. Unknown envelope version:        top-level object with
//     agent_session_state.schema_version
//     string, but the version is not
//     recognized by this binary. Returns
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
