package intent

// HandlerFailureClass is control-flow metadata. User-facing wording may change
// without changing whether the engine should continue into its context-aware,
// read-only agent lane.
type HandlerFailureClass string

const (
	HandlerFailureNone               HandlerFailureClass = ""
	HandlerFailureGenericRead        HandlerFailureClass = "generic_read"
	HandlerFailureActionableUpstream HandlerFailureClass = "actionable_upstream"
)
