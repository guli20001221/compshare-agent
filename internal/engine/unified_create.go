package engine

// SetUnifiedCreateEnabled toggles the R2b unified create-family entry.
func SetUnifiedCreateEnabled(v bool) { unifiedCreateOn = v }

// UnifiedCreateEnabled reports whether the unified create-family entry is on.
func UnifiedCreateEnabled() bool { return unifiedCreateOn }

var unifiedCreateOn bool
