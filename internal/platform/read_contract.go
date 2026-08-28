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

// ReadStatus is the control-flow outcome reported by a typed read capability.
type ReadStatus string

const (
	ReadStatusHandled            ReadStatus = "handled"
	ReadStatusNeedsInput         ReadStatus = "needs_input"
	ReadStatusFallbackBeforeTool ReadStatus = "fallback_before_tool"
	ReadStatusFailureAfterTool   ReadStatus = "failure_after_tool"
	// ReadStatusEmpty is reported when the upstream query succeeded but returned
	// no data (an empty catalog / list / no matching subject). It is distinct from
	// Handled so the Agent can tell "queried and found nothing" from "queried and
	// answered", and it is asserted as a status, never as a Chinese substring.
	ReadStatusEmpty ReadStatus = "empty"
	// ReadStatusConflict is reported when the request is ambiguous — it resolves
	// to more than one candidate subject and the read cannot pick one. It replaces
	// expressing ambiguity only through a fallback reason plus follow-up prose.
	ReadStatusConflict ReadStatus = "conflict"
	// ReadStatusUnavailable is reported by an UnavailableCapabilitySpec: the
	// capability is deliberately not backed by a real-time upstream call and
	// returns a deterministic "not available + alternatives" answer instead.
	ReadStatusUnavailable ReadStatus = "unavailable"
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
)

// RouteStatus is the dispatch outcome emitted with a typed read observation.
type RouteStatus string

const (
	RouteStatusNone                     RouteStatus = ""
	RouteStatusDispatched               RouteStatus = "dispatched"
	RouteStatusFallbackUnresolvedTarget RouteStatus = "fallback_unresolved_target"
	RouteStatusFallbackTimeWindow       RouteStatus = "fallback_time_window"
	RouteStatusFallbackIneligible       RouteStatus = "fallback_ineligible"
	RouteStatusFallbackInvalid          RouteStatus = "fallback_invalid"
	RouteStatusFailureAfterTool         RouteStatus = "failure_after_tool"
)
