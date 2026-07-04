package engine

// contextContinuationOn gates the global LLM-backed context continuation layer.
// The package variable starts false for unit-test isolation; HTTP/CLI boot
// parsers default COMPSHARE_CONTEXT_CONTINUATION to on and call the setter.
var contextContinuationOn bool

// SetContextContinuationEnabled toggles the global context continuation layer.
func SetContextContinuationEnabled(v bool) { contextContinuationOn = v }

// ContextContinuationEnabled reports whether context continuation is enabled.
func ContextContinuationEnabled() bool { return contextContinuationOn }
