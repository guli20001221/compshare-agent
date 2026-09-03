package engine

import (
	"context"
	"errors"

	"github.com/compshare-agent/internal/opscontext"
)

// ErrInstanceOpsNoSSHTarget is returned by a runner when the target instance has no
// SSH entrypoint (an empty SshLoginCommand — Windows instances, and any image without
// SSH). It is the engine-side, transport-agnostic mirror of sshops.ErrNoSSHTarget;
// the cmd adapter translates one to the other, keeping internal/sshops out of the
// engine import set. executeInstanceOps matches it with errors.Is and returns a
// bounded observation: the Guest cannot be entered through this lane, while the
// central Agent may continue with platform reads or knowledge retrieval.
var ErrInstanceOpsNoSSHTarget = errors.New("engine: instance has no SSH target")

// ErrInstanceOpsNotRunning is the engine-side mirror of sshops.ErrInstanceNotRunning:
// the instance is not in Running state, so the box cannot be entered right now and
// the reason is knowable. executeInstanceOps names the state instead of emitting the
// generic retry text. Unlike ErrInstanceOpsNoSSHTarget this IS retryable once the
// instance is running, and the message says so.
var ErrInstanceOpsNotRunning = errors.New("engine: instance is not running")

// ErrInstanceOpsNotFound is the engine-side mirror of sshops.ErrInstanceNotFound:
// the id is not in this tenant's account. Retrying it can never succeed, so
// executeInstanceOps names that instead of the generic retry text. Distinct from
// ErrInstanceOpsNotRunning (the box exists but is stopped) and from a transient
// describe failure (which keeps the retry advice).
var ErrInstanceOpsNotFound = errors.New("engine: instance not found in this account")

// ErrInstanceOpsAddressUnavailable is the engine-side mirror of a failed
// internal-address derivation. It occurs before a TCP connection or SSH session
// exists. The reply may name that layer, but must not turn the absence of guest
// evidence into a conclusion about the user's original fault.
var ErrInstanceOpsAddressUnavailable = errors.New("engine: instance internal address unavailable")

// ErrInstanceOpsSSHPreflightUnreachable mirrors a failed TCP reachability check
// after candidate addresses were derived. It is distinct from address derivation
// because the user-facing next steps differ, while still proving only that this
// diagnosis attempt never authenticated over SSH or entered the guest.
var ErrInstanceOpsSSHPreflightUnreachable = errors.New("engine: instance ssh preflight unreachable")

// InstanceOpsRunner executes ONE task-authorized in-instance diagnosis/repair and
// streams its activity back through onProgress. The engine depends only on this
// structural interface; the concrete runner (a Python Agent-SDK harness spawned
// as a subprocess that SSHes into the box) lives in internal/sshops and is wired
// in cmd. Keeping the dependency behind an interface holds the subprocess / SSH /
// paramiko dependency subtree out of internal/engine's import set (see plan §3.6).
//
// The credential is deliberately absent from InstanceOpsRequest: the runner
// fetches it from the caller's STS identity (tenant ownership is enforced
// upstream), so a plaintext secret never crosses the engine boundary.
type InstanceOpsRunner interface {
	Run(ctx context.Context, req InstanceOpsRequest, onProgress func(InstanceOpsProgress)) (InstanceOpsVerdict, error)
}

// InstanceOpsRequest is the Agent-selected, deployment-authorized request handed
// to the runner. The runner must resolve this exact ID under the caller's STS
// identity and bind credentials, audit and execution to that one instance.
// TurnID is the server-side audit and retry-dedup identity.
type InstanceOpsRequest struct {
	TurnID     string
	InstanceID string
	Task       string
	// Context is the versioned, redacted reference data for the inner agent.
	// It is independent from Task so observations cannot change the dedup hash.
	Context opscontext.Context
}

// Progress kinds emitted by a runner. Connected and command become live StepEvents; background_job
// and agent_session are internal continuation state and are never surfaced as commands. The
// terminal summary line is emitted by the engine itself from the verdict tallies, not by the runner.
const (
	// InstanceOpsProgressConnected fires once when the SSH session is established.
	InstanceOpsProgressConnected = "connected"
	// InstanceOpsProgressCommand fires once per command the harness ran or refused.
	InstanceOpsProgressCommand = "command"
	// InstanceOpsProgressBackgroundJob publishes an opaque handle before a detached launcher can
	// outlive the browser/harness. It is not a command step or an audit record.
	InstanceOpsProgressBackgroundJob = "background_job"
	// InstanceOpsProgressAgentSession carries only an opaque SDK session cursor. It is durable
	// continuation metadata, not a command, user-visible step or audit record.
	InstanceOpsProgressAgentSession = "agent_session"
)

// InstanceOpsProgress carries command metadata or an opaque job lifecycle update. It never carries
// command output (INV-6).
type InstanceOpsProgress struct {
	Kind        string // connected | command | background_job | agent_session
	Command     string // the command (Kind==command only)
	Disposition string // "ran" | "refused" | "failed" (Kind==command only)
	// Tier is the guardrail class the command was executed under ("read_only" | "mutating" |
	// "destructive"), or "" when the runner did not report one. It is the ONLY thing that separates
	// "this diagnosis looked at the box" from "this diagnosis changed it", which is the question a
	// user has after an interrupted run — and the adapter was dropping it while the audit row kept
	// it. Every consumer must treat "" as unknown rather than as read_only.
	Tier string
	// Reason names WHICH gate refused, when the runner knows: the destructive tier, the shape gate,
	// a declined/timed-out/disconnected legacy confirmation, or a command too long for the legacy wire. Empty
	// means unknown, and every consumer must degrade to the old generic wording rather than assume a
	// value.
	Reason   string
	ExitCode *int   // nil for refused/failed commands that never produced an exit status
	Bytes    int    // output byte count (metadata only; the output itself never crosses here)
	JobID    string // opaque background-job handle; never the command that created it
	JobState string // started | running | unknown | succeeded | failed | interrupted | not_found
	// JobPurpose is a short non-executable description emitted by the structured
	// job tool. The engine redacts and bounds it before SessionState persistence.
	JobPurpose string
	// AgentSession fields are populated only for Kind==InstanceOpsProgressAgentSession.
	AgentSessionID                 string
	AgentSessionWorkdirID          string
	AgentSessionContract           string
	AgentSessionModel              string
	AgentSessionConversationAnchor string
}

// InstanceOpsVerdict is the terminal diagnosis or interrupted-run report. Text is
// the harness's already-scrubbed body; Ran/Refused are independently settled
// command tallies, not a claim that the user's fault was repaired.
type InstanceOpsVerdict struct {
	Text    string
	Ran     int
	Refused int
	// AgentFailed is trusted runner metadata, never inferred from Text. Commands
	// may already have run, and their results and continuation handles remain valid.
	AgentFailed bool
	// ErrClass is the runner's bounded SDK/model failure class. Unknown classes
	// must become a generic activity code, never customer-visible free-form text.
	ErrClass string
}
