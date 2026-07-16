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
	CodecResourceRef     SlotCodecKind = "resource_ref"
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

type OperationSpec struct {
	Operation        string
	Description      string
	Fields           map[string]FieldSpec
	NeedsConfirm     bool
	Risk             security.Level
	Execution        []workflow.ExecutionStepContract
	ValidateResolved func(map[string]any) error
}

type GateContract struct {
	Executor             string         `json:"executor"`
	Risk                 security.Level `json:"risk"`
	RequiresPermission   bool           `json:"requires_permission"`
	RequiresConfirmation bool           `json:"requires_confirmation"`
	RequiresJournal      bool           `json:"requires_journal"`
}

type ConfirmationPreview struct {
	Operation string         `json:"operation"`
	Arguments map[string]any `json:"arguments"`
}

type Conflict struct {
	Slot       string          `json:"slot"`
	Candidates []SlotCandidate `json:"candidates"`
}

type ResolvedSlot struct {
	Value  any             `json:"value"`
	Source CandidateSource `json:"source"`
	Codec  SlotCodecKind   `json:"codec"`
}

type ResolvedAction struct {
	TurnID               string                           `json:"turn_id"`
	Operation            string                           `json:"operation"`
	Arguments            map[string]any                   `json:"arguments,omitempty"`
	Provenance           map[string]ResolvedSlot          `json:"provenance,omitempty"`
	Missing              []string                         `json:"missing,omitempty"`
	Conflicts            []Conflict                       `json:"conflicts,omitempty"`
	Rejected             []string                         `json:"rejected,omitempty"`
	NeedsConfirm         bool                             `json:"needs_confirm"`
	ReadyForConfirmation bool                             `json:"ready_for_confirmation"`
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
