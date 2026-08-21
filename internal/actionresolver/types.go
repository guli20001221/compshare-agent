package actionresolver

import (
	"github.com/compshare-agent/internal/security"
	"github.com/compshare-agent/internal/workflow"
)

type CandidateSource string

const (
	SourceUserExplicit     CandidateSource = "user_explicit"
	SourceVerifiedContext  CandidateSource = "verified_context"
	SourceToolObservation  CandidateSource = "tool_observation"
	SourceUserConfirmation CandidateSource = "user_confirmation"
	SourceAgentInference   CandidateSource = "agent_inference"
)

type SourceEvidence struct {
	MessageID    string `json:"message_id,omitempty"`
	ContextField string `json:"context_field,omitempty"`
	Start        int    `json:"start,omitempty"`
	End          int    `json:"end,omitempty"`
	Quote        string `json:"quote,omitempty"`
}

type SlotCandidate struct {
	Name     string          `json:"name"`
	Value    any             `json:"value"`
	Source   CandidateSource `json:"source"`
	Evidence *SourceEvidence `json:"evidence,omitempty"`
}

type ActionProposal struct {
	TurnID    string          `json:"turn_id"`
	Operation string          `json:"operation"`
	Slots     []SlotCandidate `json:"slots"`
}

type SlotCodecKind string

const (
	CodecResourceRef SlotCodecKind = "resource_ref"
	// CodecMachineType is a platform machine-type (GPU) name. It is canonicalized
	// against the LIVE catalog the engine snapshots — never against a table in
	// this repo. See MachineTypeCatalog.
	CodecMachineType SlotCodecKind = "machine_type"
	// CodecZone is an availability-zone. Like CodecMachineType it canonicalizes an
	// agent-supplied zone id or console display name against the LIVE zone catalog
	// the engine snapshots — never an alias table or city keyword. See
	// deployment.ZoneCatalogSnapshot and canonicalZone.
	CodecZone SlotCodecKind = "zone"
	// CodecImage is a CompShareImageId. It VERIFIES an explicitly-proposed image id
	// against the LIVE image catalog the engine snapshots — only a catalog-verified
	// id may pass (invariant 1), never a caller-supplied id we could not confirm. It
	// does NOT resolve a free-text ImageName (that is the workflow's recommend-and-
	// confirm job on the same snapshot). See deployment.ImageCatalogSnapshot and
	// deployment.ResolveImage.
	CodecImage           SlotCodecKind = "image"
	CodecCapacity        SlotCodecKind = "capacity"
	CodecInteger         SlotCodecKind = "integer"
	CodecNumber          SlotCodecKind = "number"
	CodecBoolean         SlotCodecKind = "boolean"
	CodecEnum            SlotCodecKind = "enum"
	CodecTime            SlotCodecKind = "time"
	CodecConstrainedText SlotCodecKind = "constrained_text"
	CodecSensitiveText   SlotCodecKind = "sensitive_text"
	CodecStructured      SlotCodecKind = "structured"
)

type FieldSpec struct {
	Name       string
	Required   bool
	Codec      SlotCodecKind
	Enum       []string
	Target     bool
	TargetKind string
}

// IntakeMode declares how an operation handles a proposal that is well-formed but
// incomplete (only Missing required fields, no conflicts/rejections/outages).
type IntakeMode string

const (
	// IntakeNone: an incomplete proposal has no guided form; the agent must ask in
	// prose (the resolver reports Missing and the model re-proposes). This is the
	// default for every operation.
	IntakeNone IntakeMode = ""
	// IntakeGuided: an incomplete proposal whose Missing fields are all collectable
	// may open a guided selection form (whitelist picks) instead of a prose
	// back-and-forth. The form collects the missing fields, then confirms.
	IntakeGuided IntakeMode = "guided"
)

// IntakeSpec is the declarative replacement for the engine hardcoding a specific
// operation name to trigger the guided card. It states whether an operation
// supports guided intake and which of its fields the guided form may collect.
type IntakeSpec struct {
	Mode              IntakeMode
	CollectableFields []string
	// DiscardableOnRejectFields are fields the form does NOT collect but whose
	// invalid value may be dropped so the form still opens. See
	// workflow.Definition.DiscardableOnRejectFields for why this is a separate
	// list from CollectableFields and never derived from the schema.
	DiscardableOnRejectFields []string
}

type OperationSpec struct {
	Operation string
	// AgentDescription is the model-facing capability boundary: what the
	// operation does and when it should (or should not) be requested. Workflow
	// execution steps deliberately live only in workflow.Definition.
	AgentDescription   string
	Fields             map[string]FieldSpec
	ImageCatalogSource string
	NeedsConfirm       bool
	Risk               security.Level
	Execution          []workflow.ExecutionStepContract
	ValidateResolved   func(map[string]any) error
	Intake             IntakeSpec
}

type GateContract struct {
	Executor             string         `json:"executor"`
	Risk                 security.Level `json:"risk"`
	RequiresPermission   bool           `json:"requires_permission"`
	RequiresConfirmation bool           `json:"requires_confirmation"`
}

type ConfirmationPreview struct {
	Operation string         `json:"operation"`
	Arguments map[string]any `json:"arguments"`
}

// RejectionKind is the TYPED reason a slot (or the whole proposal) was rejected.
// It exists so the guided-intake decision can tell a form-correctable invalid
// value apart from a structural rejection WITHOUT parsing the Rejected strings.
// Only RejectInvalidValue is form-correctable (discard the bad value, let the
// form re-collect it); every other kind must block the form.
type RejectionKind int

const (
	// RejectInvalidValue: a supplied value failed its codec (bad zone/image/enum/
	// integer, etc.). Form-correctable when the field is a declared collectable.
	RejectInvalidValue RejectionKind = iota
	// RejectUnknownOperation: the operation is not in the catalog. Never correctable.
	RejectUnknownOperation
	// RejectUnknownField: a slot names a field the operation does not have. Never
	// correctable (the form has no such input).
	RejectUnknownField
	// RejectUnknownSource: the candidate's source label is not a known source.
	RejectUnknownSource
	// RejectUnverifiedSource: a user_explicit non-target slot failed span
	// verification — a trust-boundary failure, never a "let the user re-pick" case.
	RejectUnverifiedSource
	// RejectTargetNotExist: a write target's existence could not be confirmed.
	RejectTargetNotExist
	// RejectOperationContract: the whole resolved argument set failed the
	// operation's ValidateResolved contract.
	RejectOperationContract
)

// String renders the typed rejection kind as a stable, value-free code for the
// disposition trace (the acceptance measurement reads it to attribute why a
// create proposal did not reach a card). Never includes a slot value.
func (k RejectionKind) String() string {
	switch k {
	case RejectInvalidValue:
		return "invalid_value"
	case RejectUnknownOperation:
		return "unknown_operation"
	case RejectUnknownField:
		return "unknown_field"
	case RejectUnknownSource:
		return "unknown_source"
	case RejectUnverifiedSource:
		return "unverified_source"
	case RejectTargetNotExist:
		return "target_not_exist"
	case RejectOperationContract:
		return "operation_contract"
	default:
		return "unknown"
	}
}

// RejectedProblem is the typed twin of one Rejected[] entry. Slot is the field
// name ("" for operation-level rejections). Populated in lockstep with Rejected
// so the two never diverge; internal to the resolver→engine intake decision, not
// part of the model-facing JSON.
type RejectedProblem struct {
	Slot string
	Kind RejectionKind
}

// Conflict is a slot the resolver refuses to decide. Two shapes reach it:
// Candidates is set when sources disagree on a value; CatalogCandidates is set
// when the value matched several live catalog entries. Both mean the same thing
// to the caller — the agent must ask, the server must not guess.
type Conflict struct {
	Slot              string          `json:"slot"`
	Candidates        []SlotCandidate `json:"candidates,omitempty"`
	CatalogCandidates []string        `json:"catalog_candidates,omitempty"`
	Reason            string          `json:"reason,omitempty"`
}

type ResolvedSlot struct {
	Value  any             `json:"value"`
	Source CandidateSource `json:"source"`
	Codec  SlotCodecKind   `json:"codec"`
}

// ResolvedAction is the adjudicated proposal. The four refusal channels are
// distinct on purpose and must not be collapsed: Missing means the user has not
// said it yet, Conflicts means several readings are defensible, Rejected means
// the value is invalid, and DependencyFailures means the SERVER could not obtain
// a fact it needs to decide (e.g. the live machine-type catalog). Only the last
// is a server-side outage — reporting it as a rejection would blame the user for
// our own failed query, and reporting it as success would mean guessing.
type ResolvedAction struct {
	TurnID     string                  `json:"turn_id"`
	Operation  string                  `json:"operation"`
	Arguments  map[string]any          `json:"arguments,omitempty"`
	Provenance map[string]ResolvedSlot `json:"provenance,omitempty"`
	Missing    []string                `json:"missing,omitempty"`
	Conflicts  []Conflict              `json:"conflicts,omitempty"`
	Rejected   []string                `json:"rejected,omitempty"`
	// RejectedProblems is the typed twin of Rejected (same entries, with a Kind),
	// used only by the guided-intake decision. json:"-" — the model already gets
	// the human-readable Rejected[]; this is an internal classification.
	RejectedProblems     []RejectedProblem                `json:"-"`
	DependencyFailures   []string                         `json:"dependency_failures,omitempty"`
	NeedsConfirm         bool                             `json:"needs_confirm"`
	ReadyForConfirmation bool                             `json:"ready_for_confirmation"`
	ReadyForIntake       bool                             `json:"ready_for_intake"`
	Confirmation         *ConfirmationPreview             `json:"confirmation,omitempty"`
	Gate                 GateContract                     `json:"gate"`
	Execution            []workflow.ExecutionStepContract `json:"execution"`
}

// EvidenceVerifier is the trust boundary between model-authored provenance and
// server-owned context. A candidate cannot make itself trusted by labelling its
// own source as user_explicit.
type EvidenceVerifier interface {
	VerifyCandidate(SlotCandidate) bool
}

type EvidenceVerifierFunc func(SlotCandidate) bool

func (f EvidenceVerifierFunc) VerifyCandidate(candidate SlotCandidate) bool { return f(candidate) }

// TargetVerdict is the disposition of a write TARGET, decided by the engine (which
// owns the selection binding and the existence network) and consumed by the pure
// resolver. It keeps the resolver's four refusal channels honest: a target that
// cannot be verified is not uniformly "rejected" — an outage is a DependencyFailure
// and a genuine ambiguity is a Conflict.
type TargetVerdict int

const (
	// TargetReject: no verifiable existence — refuse (an unselected/absent id).
	TargetReject TargetVerdict = iota
	// TargetAccept: exists in the account this turn, no conflict — the target may
	// reach the confirmation card (the user-confirm event authorizes execution).
	TargetAccept
	// TargetConflict: the user's own references disagree — the agent must ask.
	TargetConflict
	// TargetDependencyFailure: existence could not be verified (upstream outage) —
	// a server-side failure, never the user's target being invalid.
	TargetDependencyFailure
)

// TargetAdjudicator, when implemented by the resolver's verifier, decides a write
// target's disposition. The resolver uses it INSTEAD of the plain bool verify for
// target fields, so existence outages and reference conflicts land in the right
// channel. A verifier that does not implement it falls back to VerifyCandidate.
type TargetAdjudicator interface {
	AdjudicateTarget(SlotCandidate) TargetVerdict
}
