package platform

// ReadRequest is implemented by one request type per read capability. The model
// schema, the JSON decoder, the validator and the handler all use the same
// concrete type — a read handler never receives the user's raw sentence, only
// its own typed request.
type ReadRequest interface {
	MissingFields() []MissingField
}

// MissingField names a required request field the caller omitted. It is a
// structured validation state, asserted directly — never a Chinese substring.
type MissingField struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Missing builds a MissingField for a required-but-absent field.
func Missing(name string) MissingField { return MissingField{Name: name, Reason: "required"} }

// ReadStatus is the control-flow outcome a read capability reports. The values
// match the legacy handler-status wire strings so a migrated capability's tool
// observation is byte-identical to the pre-migration one.
type ReadStatus string

const (
	ReadStatusHandled            ReadStatus = "handled"
	ReadStatusNeedsInput         ReadStatus = "needs_input"
	ReadStatusFallbackBeforeTool ReadStatus = "fallback_before_tool"
	ReadStatusFailureAfterTool   ReadStatus = "failure_after_tool"
)

// ReadFailureClass classifies a post-tool failure. It is control-flow metadata:
// user-facing wording may change without changing whether the engine continues
// into its read-only agent lane.
type ReadFailureClass string

const (
	ReadFailureNone               ReadFailureClass = ""
	ReadFailureGenericRead        ReadFailureClass = "generic_read"
	ReadFailureActionableUpstream ReadFailureClass = "actionable_upstream"
)

// ReadFallbackReason names why a read fell back before calling any tool.
type ReadFallbackReason string

const (
	ReadFallbackNone             ReadFallbackReason = ""
	ReadFallbackMissingTarget    ReadFallbackReason = "missing_target"
	ReadFallbackUnresolvedTarget ReadFallbackReason = "unresolved_target"
	ReadFallbackAmbiguousTarget  ReadFallbackReason = "ambiguous_target"
	ReadFallbackTimeWindow       ReadFallbackReason = "time_window"
	ReadFallbackValidation       ReadFallbackReason = "validation"
	ReadFallbackActionNotAllowed ReadFallbackReason = "action_not_allowed"
)
