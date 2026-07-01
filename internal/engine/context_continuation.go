package engine

// contextContinuationOn gates the global LLM-backed context continuation layer.
// Default-off: it can route short follow-ups into mutating workflow confirmation
// paths, so rollout must be eval-gated and explicitly enabled at boot.
var contextContinuationOn bool

// SetContextContinuationEnabled toggles the global context continuation layer.
func SetContextContinuationEnabled(v bool) { contextContinuationOn = v }

// ContextContinuationEnabled reports whether context continuation is enabled.
func ContextContinuationEnabled() bool { return contextContinuationOn }
