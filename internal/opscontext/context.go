// Package opscontext defines the versioned, non-secret reference context sent to
// the in-instance operations agent. It is intentionally a leaf package: engine
// produces user-originated context while sshops adds a narrow projection of
// control-plane facts, and neither package may depend on the other.
package opscontext

const (
	// SchemaVersion is the current wire contract for contextual SSH diagnosis.
	// A zero value means no contextual payload was requested, preserving the
	// legacy task-only handshake for direct callers.
	//
	// v2 (2026-08-14) exists because v1 shipped ONE fact, instance.reported_ports,
	// holding two things that must never be conflated: DescribeCompShareInstance's
	// Ports block and its TcpForwards list. "A port is configured" and "the platform
	// forwards that port" are different claims with different failure modes, and a
	// single key invited the model to read either as the other — and as evidence of a
	// guest listener, which neither is. v2 splits them and adds what the lane could
	// state but never did: the software this instance declares, and the image
	// catalog's EXPECTED port for that software.
	//
	// Version compatibility is deliberately asymmetric. An older harness rejects an
	// unknown version and degrades to a task-only run — no facts, but no misread
	// facts either. A newer harness still accepts v1, so a rollback of the server
	// binary alone does not silently strip the context.
	SchemaVersion = 2

	// SchemaVersionPortsMerged is v1, kept named because the harness must keep
	// accepting it during a mixed deploy, not because anything still produces it.
	SchemaVersionPortsMerged = 1

	StatusKnown       = "known"
	StatusUnknown     = "unknown"
	StatusNotObserved = "not_observed"
	StatusReported    = "reported"
)

// Fact-coverage bits are audit metadata only. They deliberately describe which
// server facts were supplied, not their values, so the audit never becomes a
// second copy of platform data or user conversation.
//
// New bits are appended, never reordered: the value is persisted in
// ssh_ops_audit.context_fact_coverage and a shifted bit would silently rewrite
// the meaning of every row already stored.
const (
	CoverageInstance uint32 = 1 << iota
	CoverageGPU
	CoverageImage
	CoverageDisk
	// CoveragePortHints is the Describe Ports block ALONE. It was CoveragePorts in
	// v1, where it also stood for the forwards; a row written by that producer
	// therefore over-claims this bit, which is why the version is stored beside it.
	CoveragePortHints
	CoverageMonitor
	CoverageTCPForwards
	CoverageSoftware
	CoverageCatalogPorts
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
