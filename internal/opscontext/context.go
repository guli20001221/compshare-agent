// Package opscontext defines the versioned, non-secret reference context sent to
// the in-instance operations agent. It is intentionally a leaf package: engine
// produces user-originated context while sshops adds a narrow projection of
// control-plane facts, and neither package may depend on the other.
package opscontext

const (
	// SchemaVersion is the first wire contract for contextual SSH diagnosis.
	// A zero value means no contextual payload was requested, preserving the
	// legacy task-only handshake for direct callers.
	SchemaVersion = 1

	StatusKnown       = "known"
	StatusUnknown     = "unknown"
	StatusNotObserved = "not_observed"
	StatusReported    = "reported"
)

// Fact-coverage bits are audit metadata only. They deliberately describe which
// server facts were supplied, not their values, so the audit never becomes a
// second copy of platform data or user conversation.
const (
	CoverageInstance uint32 = 1 << iota
	CoverageGPU
	CoverageImage
	CoverageDisk
	CoveragePorts
	CoverageMonitor
)

// Context is independent from the planner-produced Task. Keeping the two
// separate preserves task hashing and durable replay semantics: a changing
// monitor value or observation timestamp cannot turn one task into another.
//
// Every item that can influence the agent carries source, observed_at, and
// status. "unknown" and "not_observed" are first-class values rather than
// omitted fields, so absence is never silently interpreted as a healthy state.
type Context struct {
	SchemaVersion     int          `json:"schema_version"`
	CurrentUserReport *UserReport  `json:"current_user_report,omitempty"`
	PriorUserReports  []UserReport `json:"prior_user_reports,omitempty"`
	PlatformFacts     []Fact       `json:"platform_facts,omitempty"`
	Coverage          uint32       `json:"-"`
}

// Enabled reports whether this payload uses the currently supported schema.
func (c Context) Enabled() bool { return c.SchemaVersion == SchemaVersion }

// UserReport is direct user-authored text, redacted before it crosses the
// engine-to-agent boundary. Assistant prose is deliberately not represented:
// it can contain an outer agent's unsupported inferences or proposed commands.
type UserReport struct {
	Text       string `json:"text"`
	Source     string `json:"source"`
	ObservedAt string `json:"observed_at"`
	Status     string `json:"status"`
}

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
