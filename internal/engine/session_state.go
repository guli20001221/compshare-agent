package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/compshare-agent/internal/knowledge"
)

// SessionStateSchemaV1 is the first persisted JSON schema version for SessionState.
const SessionStateSchemaV1 = "1.0"

// SessionStateSchemaV2 adds a durable context frame. Old records still load, but
// new writes use v2 so older binaries fail closed instead of silently dropping
// the frame during a rolling deployment.
const SessionStateSchemaV2 = "2.0"

// SessionStateSchemaV3 extends ContextFrame from create/deploy-only fields to
// generic workflow task slots. Older binaries must fail closed rather than
// dropping pending workflow parameters on write-back.
const SessionStateSchemaV3 = "3.0"

// SessionStateSchemaV4 records the trust source for selected instances and
// workflow task slots. Older binaries must fail closed instead of silently
// dropping the source and later treating an observed instance as user-selected.
const SessionStateSchemaV4 = "4.0"

// SessionStateSchemaV5 adds durable semantic memory and explicit freshness.
// TaskSnapshot is a safe, compact projection; raw tool transcripts are
// deliberately not part of this schema. It also added ConversationDigest, which
// this binary no longer has — a V5 row still decodes, and that field is dropped.
const SessionStateSchemaV5 = "5.0"

// SessionStateSchemaV6 persists bounded evidence from answers that passed the
// semantic knowledge verifier. It lets a cold/restarted agent validate a short
// follow-up against the same source instead of trusting arbitrary assistant text
// or forcing another retrieval.
const SessionStateSchemaV6 = "6.0"

// SessionStateSchemaV7 adds provenance-bearing long-term memory and bounded
// verbatim excerpts for compaction failure. Both live in the existing context
// envelope, so they commit atomically with the turn and require no DB migration.
const SessionStateSchemaV7 = "7.0"

const SessionStateSchemaCurrent = SessionStateSchemaV7

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
	SessionStateSchemaV2: {},
	SessionStateSchemaV3: {},
	SessionStateSchemaV4: {},
	SessionStateSchemaV5: {},
	SessionStateSchemaV6: {},
	SessionStateSchemaV7: {},
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
// M1 shipped with 4 scalar fields. M2 added RecentFacts. M3.1 added
// LastStockGpuModel. M4 added PendingSelection* so an instance list can be
// resolved by ordinal/name/ID in a later turn. known_constraints /
// pending_action remain deferred.
//
// LastStockGpuModel is the legacy stock referent used only when
// USE_SESSION_FACT_CONTEXT is disabled or the engine is running without
// persisted session facts. With fact context enabled, stock follow-ups use
// RecentFacts StockSnapshot entries instead.
type SessionState struct {
	SchemaVersion                   string                  `json:"schema_version"`
	SelectedInstanceID              string                  `json:"selected_instance_id,omitempty"`
	SelectedInstanceName            string                  `json:"selected_instance_name,omitempty"`
	SelectedInstanceSource          string                  `json:"selected_instance_source,omitempty"`
	SelectedInstanceAtUnix          int64                   `json:"selected_instance_at_unix,omitempty"`
	SelectedInstanceFreshness       string                  `json:"selected_instance_freshness,omitempty"`
	LastStockGpuModel               string                  `json:"last_stock_gpu_model,omitempty"`
	PendingSelectionKind            string                  `json:"pending_selection_kind,omitempty"`
	PendingSelectionIntent          string                  `json:"pending_selection_intent,omitempty"`
	PendingSelectionOriginalUserMsg string                  `json:"pending_selection_original_user_msg,omitempty"`
	PendingSelectionCreatedTurn     int                     `json:"pending_selection_created_turn,omitempty"`
	PendingSelectionProducedAtUnix  int64                   `json:"pending_selection_produced_at_unix,omitempty"`
	PendingSelectionTTLSeconds      int                     `json:"pending_selection_ttl_seconds,omitempty"`
	PendingSelectionTruncated       bool                    `json:"pending_selection_truncated,omitempty"`
	PendingSelectionTotalCount      int                     `json:"pending_selection_total_count,omitempty"`
	PendingSelectionItems           []PendingSelectionItem  `json:"pending_selection_items,omitempty"`
	ContextFrame                    ContextFrame            `json:"context_frame,omitempty"`
	RecentFacts                     []ToolFact              `json:"recent_facts,omitempty"`
	TaskSnapshot                    TaskSnapshot            `json:"task_snapshot,omitempty"`
	VerifiedKnowledge               []VerifiedKnowledgeTurn `json:"verified_knowledge,omitempty"`
}

// VerifiedKnowledgeTurn is compact, durable provenance for an answer that
// already passed the semantic evidence verifier. It is reference-only memory:
// it can support later read-only prose, never authorize writes or satisfy a
// real-time state query.
type VerifiedKnowledgeTurn struct {
	Question       string                   `json:"question,omitempty"`
	Answer         string                   `json:"answer,omitempty"`
	Evidence       knowledge.EvidenceLedger `json:"evidence"`
	VerifiedAtUnix int64                    `json:"verified_at_unix,omitempty"`
}

const (
	// SelectedInstanceSourceObserved marks a current-instance binding recorded
	// from a tool observation (a read saw it). It is understanding-only — it helps
	// resolve who "它" is — and is NEVER a selection proof for a write target.
	SelectedInstanceSourceObserved = "observed"
	// SelectedInstanceSourceUser marks a binding the user genuinely established
	// this session — an explicit id they typed, or a pick from a shown selection
	// card. Only this (and a shown-card pick / the account's sole instance) is a
	// SelectionProof; a later turn's "关掉它" may re-verify its existence and act.
	SelectedInstanceSourceUser = "user_selected"

	ContextFrameKindCreate       = "create_instance"
	ContextFrameKindDeploy       = "deploy_model"
	ContextFrameKindWorkflowTask = "workflow_task"

	ContextFrameStatusPending           = "pending"
	ContextFrameStatusFailedRecoverable = "failed_recoverable"

	ContextFrameTTLSeconds = 300
)

// ContextFrame is the single carried task state for short follow-ups like
// "那华北二A呢" or "200G". It stores the user's current pending task and the
// last recoverable failure so the next turn can update generic parameters
// without per-domain continuation branches. It intentionally carries only
// non-secret preferences and display labels; API-only fields such as
// zone_id/az_group never belong here.
type ContextFrame struct {
	Version          int                `json:"version,omitempty"`
	Kind             string             `json:"kind,omitempty"`
	Status           string             `json:"status,omitempty"`
	Intent           string             `json:"intent,omitempty"`
	Workflow         string             `json:"workflow,omitempty"`
	OriginalUserMsg  string             `json:"original_user_msg,omitempty"`
	Slots            map[string]string  `json:"slots,omitempty"`
	SlotSources      map[string]string  `json:"slot_sources,omitempty"`
	MissingSlots     []string           `json:"missing_slots,omitempty"`
	GPU              string             `json:"gpu,omitempty"`
	ImagePref        string             `json:"image_pref,omitempty"`
	ImageSource      string             `json:"image_source,omitempty"`
	Workload         string             `json:"workload,omitempty"`
	Zone             string             `json:"zone,omitempty"`
	ZoneLabel        string             `json:"zone_label,omitempty"`
	Stage            string             `json:"stage,omitempty"`
	FailureReason    string             `json:"failure_reason,omitempty"`
	AlternativeZones []ContextFrameZone `json:"alternative_zones,omitempty"`
	CreatedTurn      int                `json:"created_turn,omitempty"`
	ProducedAtUnix   int64              `json:"produced_at_unix,omitempty"`
	TTLSeconds       int                `json:"ttl_seconds,omitempty"`
	Freshness        string             `json:"freshness,omitempty"`
}

type ContextFrameZone struct {
	Zone  string `json:"zone,omitempty"`
	Label string `json:"label,omitempty"`
}

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
	Kind            string         `json:"kind"`
	SubjectID       string         `json:"subject_id"`
	Payload         map[string]any `json:"payload,omitempty"`
	Source          string         `json:"source,omitempty"`
	Completeness    string         `json:"completeness,omitempty"`
	Freshness       string         `json:"freshness,omitempty"`
	RefreshRequired bool           `json:"refresh_required,omitempty"`
	ProducedAtTurn  int            `json:"produced_at_turn"`
	ProducedAtUnix  int64          `json:"produced_at_unix"`
	TTLSeconds      int            `json:"ttl_seconds,omitempty"`
}

// ToolFact kind constants. New kinds must be added here, in
// ttlSecondsForKind, and in the round-trip test's expected-keys map.
const (
	FactKindInstanceState = "instance_state"
	FactKindMonitorSample = "monitor_sample"
	FactKindStockSnapshot = "stock_snapshot"
	FactKindPriceQuote    = "price_quote"
	FactKindBillingQuote  = "billing_quote"
)

// Per-kind TTL constants. Facts are descriptive same-session context ("刚才那个
// CPU 高是什么意思" / "我们在看哪台实例"), so the window is set to 5 minutes — long
// enough to survive a normal multi-turn conversation about one instance without
// the agent forgetting which instance / its basic state. Volatile metric freshness
// is NOT relied on here: a "现在还高吗" follow-up re-queries via monitor routing
// refresh logic, so a stale monitor sample is never
// presented as the authoritative current value — it is advisory context only.
const (
	factTTLSecondsInstanceState = 300
	factTTLSecondsMonitorSample = 300
	factTTLSecondsStockSnapshot = 300
	factTTLSecondsPriceQuote    = 300
	factTTLSecondsBillingQuote  = 300
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
	case FactKindStockSnapshot:
		return factTTLSecondsStockSnapshot
	case FactKindPriceQuote:
		return factTTLSecondsPriceQuote
	case FactKindBillingQuote:
		return factTTLSecondsBillingQuote
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
	case FactKindStockSnapshot:
		return map[string]struct{}{
			"model":  {},
			"status": {},
			"zone":   {},
			"count":  {},
			"enough": {},
			"action": {},
		}
	case FactKindPriceQuote:
		return map[string]struct{}{
			"action":         {},
			"gpu_type":       {},
			"zone":           {},
			"charge_type":    {},
			"price":          {},
			"original_price": {},
			"target":         {},
		}
	case FactKindBillingQuote:
		return map[string]struct{}{
			"action":      {},
			"resource_id": {},
			"amount":      {},
			"target":      {},
			"note":        {},
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
		s.SchemaVersion = SessionStateSchemaCurrent
	}
	type alias SessionState
	raw, err := json.Marshal(alias(s))
	if err != nil {
		return nil, err
	}
	if contextFrameEmpty(s.ContextFrame) {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		delete(m, "context_frame")
		if taskSnapshotEmpty(s.TaskSnapshot) {
			delete(m, "task_snapshot")
		}
		return json.Marshal(m)
	}
	if !taskSnapshotEmpty(s.TaskSnapshot) {
		return raw, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if taskSnapshotEmpty(s.TaskSnapshot) {
		delete(m, "task_snapshot")
	}
	return json.Marshal(m)
}

func contextFrameEmpty(f ContextFrame) bool {
	return f.Version == 0 &&
		f.Kind == "" &&
		f.Status == "" &&
		f.Intent == "" &&
		f.Workflow == "" &&
		f.OriginalUserMsg == "" &&
		len(f.Slots) == 0 &&
		len(f.SlotSources) == 0 &&
		len(f.MissingSlots) == 0 &&
		f.GPU == "" &&
		f.ImagePref == "" &&
		f.ImageSource == "" &&
		f.Workload == "" &&
		f.Zone == "" &&
		f.ZoneLabel == "" &&
		f.Stage == "" &&
		f.FailureReason == "" &&
		len(f.AlternativeZones) == 0 &&
		f.CreatedTurn == 0 &&
		f.ProducedAtUnix == 0 &&
		f.TTLSeconds == 0 &&
		f.Freshness == ""
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
	f = normalizeToolFactForStore(f)
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
