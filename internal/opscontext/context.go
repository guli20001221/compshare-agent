// Package opscontext defines the versioned model-visible reference context and
// its request-local private capabilities for the in-instance operations agent.
// Private fields are excluded from Context JSON and travel only in explicitly
// separate supervisor handshake fields. The package is intentionally a leaf:
// engine produces user context while sshops adds a narrow control-plane projection.
package opscontext

import "strconv"

const (
	// SchemaVersion is the current SSH context wire contract. Version 4 adds a
	// control-plane-authoritative instance kind (vm or pod). It deliberately
	// derives that classification from the resource-ID contract, rather than
	// from the guest image or process topology.
	SchemaVersion = 4

	// SchemaVersionRoleComplete is v3, retained during a mixed deployment. It
	// carried the canonical role-preserving outer conversation but not the
	// authoritative instance kind fact.
	SchemaVersionRoleComplete = 3

	// SchemaVersionUserOnly is v2, retained because the harness accepts it during
	// a mixed deploy. It carried current/prior user reports but no assistant side.
	SchemaVersionUserOnly = 2

	// SchemaVersionPortsMerged is v1, kept named because the harness must keep
	// accepting it during a mixed deploy, not because anything still produces it.
	SchemaVersionPortsMerged = 1

	// AgentSessionContract is the prompt/tool/context contract bound to an opaque
	// Claude SDK continuation cursor. Version 3 includes the autonomous repair-
	// closure semantics; a v2 transcript must start fresh rather than inherit the
	// former stop-before-runtime-verification behavior. All transport layers compare
	// this one value.
	AgentSessionContract = "sshops-agent-v3"

	StatusKnown       = "known"
	StatusUnknown     = "unknown"
	StatusNotObserved = "not_observed"
	StatusReported    = "reported"
)

// Fact-coverage bits are audit metadata only. They deliberately describe which
// server facts were supplied, not their values, so the audit never becomes a
// second copy of platform data or user conversation.
//
// New bits are appended, never reordered: the value is persisted in
// ssh_ops_audit.context_fact_coverage and a shifted bit would silently rewrite
// the meaning of every row already stored.
const (
	CoverageInstance uint32 = 1 << iota
	CoverageGPU
	CoverageImage
	CoverageDisk
	// CoveragePortHints is the Describe Ports block ALONE. It was CoveragePorts in
	// v1, where it also stood for the forwards; a row written by that producer
	// therefore over-claims this bit, which is why the version is stored beside it.
	CoveragePortHints
	CoverageMonitor
	CoverageTCPForwards
	CoverageSoftware
	// CoverageCatalogPorts means the catalog was correlated to this instance's declared software.
	// CoverageRegionPortHints means it could not be, and the model received a region-wide list under
	// a name that says so. Separate bits because the two support different conclusions.
	CoverageCatalogPorts
	CoverageRegionPortHints
	// CoverageInstanceKind means the model received the control-plane resource
	// kind. It is distinct from image type: an UHost created from a container
	// image is still a VM for access and port semantics.
	CoverageInstanceKind
)

// Context is independent from the planner-produced Task. Keeping the two
// separate preserves task hashing and retry-dedup semantics: a changing
// monitor value or observation timestamp cannot turn one task into another.
//
// Every item that can influence the agent carries source, observed_at, and
// status. "unknown" and "not_observed" are first-class values rather than
// omitted fields, so absence is never silently interpreted as a healthy state.
type Context struct {
	SchemaVersion       int                   `json:"schema_version"`
	ConversationHistory []ConversationMessage `json:"conversation_history,omitempty"`
	PlatformFacts       []Fact                `json:"platform_facts,omitempty"`
	Coverage            uint32                `json:"-"`
	// EndpointTargets are private, server-selected capabilities for the endpoint probe tool. They
	// are deliberately excluded from Context's JSON representation: URL query strings can contain
	// live console tokens and hosts are outside the model-visible allowlist. Supervisor serializes
	// this slice under its own stdin-only handshake key, while the model receives only each target's
	// opaque ID and non-secret label through the MCP tool schema.
	EndpointTargets []EndpointTarget `json:"-"`
	// ProbeAuthorizations are current-request Authorization values retained behind
	// opaque references for the two structured endpoint probes. Like SSH passwords
	// and private endpoint targets, values travel only in the supervisor's stdin
	// handshake and are never part of Context JSON, prompts, confirmations, audit,
	// session state, or durable replay. References expire with the harness process.
	ProbeAuthorizations []ProbeAuthorization `json:"-"`
	// PendingBackgroundJob is an opaque handle produced by the reviewed guest job tool. It is
	// session-state continuity, not conversation memory and not a command: the supervisor sends it
	// on a separate handshake field so it cannot change the versioned reference-context schema.
	// A resumed harness may only poll this handle; it never receives the original command.
	PendingBackgroundJob *BackgroundJob `json:"-"`
	// BackgroundJobSlotBusy is true when this conversation already tracks an unresolved job on a
	// different instance. The harness receives only this boolean, never that instance's ID or handle,
	// and uses it solely to refuse a second untrackable background launch. Reads and separately
	// approved foreground repairs remain available.
	BackgroundJobSlotBusy bool `json:"-"`
	// RepairScopeAuthorized records the server-side entry authorization for this one lane run. It is
	// never model input and never persisted by this package. The harness uses it only to distinguish
	// the current task-scoped repair contract from an older caller that still requires one approval
	// round-trip per guest mutation. Destructive/form/control-plane gates remain independent.
	RepairScopeAuthorized bool `json:"-"`
	// AgentSession is an opaque SDK continuation cursor owned by the current product session and
	// target instance. It contains no transcript, command, output or credential. The harness may
	// resume it only under the same contract/model and stable control-plane working directory.
	AgentSession *AgentSession `json:"-"`
	// BridgeConversationAnchor is the digest of the complete outer conversation in this request.
	// It travels beside the SDK cursor and is echoed only after the new harness proves the context
	// reached a genuine model turn. It never enters the model prompt or audit payload.
	BridgeConversationAnchor string `json:"-"`
}

// AgentSession is the minimum private state needed to let the one-shot SDK harness resume its own
// local transcript after a browser disconnect or Engine rebuild. SessionID is a UUID generated by
// the server; Contract and Model prevent stale prompts/tool surfaces from being resumed silently.
type AgentSession struct {
	// SessionID is the last server-committed SDK transcript for a resume, or the desired
	// transcript ID for a fresh run. AttemptSessionID is populated only for a resume: the
	// harness forks SessionID into this new ID so a failed attempt cannot append uncommitted
	// prompt/tool history to the durable transcript. Only AttemptSessionID may be receipted.
	SessionID        string `json:"session_id"`
	AttemptSessionID string `json:"attempt_session_id,omitempty"`
	// WorkdirID is a stable opaque UUID for the Claude project directory across successful
	// forks. It is deliberately independent of the changing transcript ID.
	WorkdirID          string `json:"workdir_id"`
	Contract           string `json:"contract"`
	Model              string `json:"model,omitempty"`
	Resume             bool   `json:"resume"`
	ConversationAnchor string `json:"conversation_anchor,omitempty"`
}

// BackgroundJob is the minimum state needed to continue observing a long guest operation after
// the browser disconnected. JobID is opaque, State is only a lifecycle hint, and Purpose is a
// redacted bounded description for continuity. None is sufficient to replay the command that
// created the job, which is deliberately not retained here.
type BackgroundJob struct {
	JobID   string `json:"job_id"`
	State   string `json:"state"`
	Purpose string `json:"purpose,omitempty"`
}

// Enabled reports whether this payload uses the currently supported schema.
func (c Context) Enabled() bool { return c.SchemaVersion == SchemaVersion }

// ConversationMessage is one endpoint of a committed outer conversation turn.
// It deliberately carries only the role and the already-redacted visible text:
// raw outer tool transcripts remain outside this context because current platform
// facts have their own typed projection below.
type ConversationMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

const (
	ConversationRoleUser      = "user"
	ConversationRoleAssistant = "assistant"
)

// Fact is an allowlisted control-plane observation. Value is limited by the
// producer to JSON-compatible scalar/map/list data; it must never be a raw
// Describe response because those can contain SSH endpoints and credentials.
type Fact struct {
	Key        string `json:"key"`
	Value      any    `json:"value"`
	Source     string `json:"source"`
	ObservedAt string `json:"observed_at"`
	Status     string `json:"status"`
}

// EndpointTarget is one control-plane-selected endpoint the harness may probe from its own network
// vantage. It is never accepted from model input: the model supplies only ID and the harness resolves
// it against this list. URL and Host therefore remain private transport data, not prompt context or
// audit data. Kind is "http" or "tcp".
type EndpointTarget struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Source string `json:"source"`
	URL    string `json:"url,omitempty"`
	Host   string `json:"host,omitempty"`
	Port   int    `json:"port,omitempty"`
}

// ProbeAuthorization binds one model-safe current-request reference to its
// private exact HTTP Authorization header value. Reference is not a credential
// and has no meaning outside the short-lived harness that received this record.
type ProbeAuthorization struct {
	Reference string `json:"ref"`
	Value     string `json:"value"`
}

func (p ProbeAuthorization) String() string {
	return "ProbeAuthorization{Reference:" + strconv.Quote(p.Reference) + ", Value:[REDACTED]}"
}

func (p ProbeAuthorization) GoString() string { return p.String() }
