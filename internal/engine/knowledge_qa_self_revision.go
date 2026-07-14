package engine

// kqaSelfRevisionOn preserves the deployed anti-over-conservatism policy in the
// unified proof-carrying repair. When enabled, the repair prompt asks for a direct
// conclusion whenever evidence clearly supports it, but the returned answer still
// has to carry and pass the same exact claim proof. Keeping it in the same call
// avoids the old prose-only second revision, which could change the answer after
// grounding validation and then required a further model call to verify safely.
// The Go-package default remains false; deployment config sets it at boot.
var kqaSelfRevisionOn bool

// SetKQASelfRevisionEnabled toggles directness guidance in the bounded knowledge
// repair. Boot-only, mirroring SetDisciplinedKnowledgeQASynthesisEnabled.
func SetKQASelfRevisionEnabled(v bool) { kqaSelfRevisionOn = v }

// KQASelfRevisionEnabled reports whether directness guidance is on.
func KQASelfRevisionEnabled() bool { return kqaSelfRevisionOn }
