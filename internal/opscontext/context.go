// Package opscontext defines the versioned, non-secret reference context sent to
// the in-instance operations agent. It is intentionally a leaf package: engine
// produces user-originated context while sshops adds a narrow projection of
// control-plane facts, and neither package may depend on the other.
package opscontext

const (
	// SchemaVersion is the current SSH context wire contract. Version 2 keeps
	// configured ports, TCP forwards, declared software and catalog hints distinct.
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
	// CoverageCatalogPorts means the catalog was correlated to this instance's declared software.
	// CoverageRegionPortHints means it could not be, and the model received a region-wide list under
	// a name that says so. Separate bits because the two support different conclusions.
	CoverageCatalogPorts
	CoverageRegionPortHints
)

// Context is independent from the planner-produced Task. Keeping the two
// separate preserves task hashing and retry-dedup semantics: a changing
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
	// EndpointTargets are private, server-selected capabilities for the endpoint probe tool. They
	// are deliberately excluded from Context's JSON representation: URL query strings can contain
	// live console tokens and hosts are outside the model-visible allowlist. Supervisor serializes
	// this slice under its own stdin-only handshake key, while the model receives only each target's
	// opaque ID and non-secret label through the MCP tool schema.
	EndpointTargets []EndpointTarget `json:"-"`
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

// EndpointTarget is one control-plane-selected endpoint the harness may probe from its own network
// vantage. It is never accepted from model input: the model supplies only ID and the harness resolves
// it against this list. URL and Host therefore remain private transport data, not prompt context or
// audit data. Kind is "http" or "tcp".
type EndpointTarget struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Source string `json:"source"`
	URL    string `json:"url,omitempty"`
	Host   string `json:"host,omitempty"`
	Port   int    `json:"port,omitempty"`
}
