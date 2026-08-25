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
// engine import set. executeInstanceOps matches it with errors.Is to give an honest,
// NON-retryable refusal instead of the generic "please retry" text.
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

// InstanceOpsRunner executes ONE consented, read-only in-instance diagnosis and
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

// InstanceOpsRequest is the resolved, user-consented request handed to the runner.
// TurnID is the server-side audit and retry-dedup identity.
type InstanceOpsRequest struct {
	TurnID     string
	InstanceID string
	Task       string
	// Context is the versioned, redacted reference data for the inner agent.
	// It is independent from Task so observations cannot change the dedup hash.
	Context opscontext.Context
	// ConfirmWrite asks the user about ONE command that will change the box, and blocks until they
	// answer. It is separate from the lane-level card: that one authorizes entering the instance and
	// never names what will change, so it cannot stand as consent for `kill 6934`. The terminal
	// reason matters just as much as the boolean: a timeout, a disconnect and an explicit decline
	// all keep the command unexecuted, but require different user-facing guidance. nil means no
	// human is reachable, and the runner must then refuse rather than proceed — see sshops.Service.
	ConfirmWrite func(command string) ConfirmationResult
}

// Progress kinds emitted by a runner. Connected and command become live StepEvents; background_job
// is internal live-session continuity and is never surfaced as a command. The terminal summary line
// is emitted by the engine itself from the verdict tallies, not by the runner.
const (
	// InstanceOpsProgressConnected fires once when the SSH session is established.
	InstanceOpsProgressConnected = "connected"
	// InstanceOpsProgressCommand fires once per command the harness ran or refused.
	InstanceOpsProgressCommand = "command"
	// InstanceOpsProgressBackgroundJob publishes an opaque handle before a detached launcher can
	// outlive the browser/harness. It is not a command step or an audit record.
	InstanceOpsProgressBackgroundJob = "background_job"
)

// InstanceOpsProgress carries command metadata or an opaque job lifecycle update. It never carries
// command output (INV-6).
type InstanceOpsProgress struct {
	Kind        string // connected | command | background_job
	Command     string // the command (Kind==command only)
	Disposition string // "ran" | "refused" | "failed" (Kind==command only)
	// Tier is the guardrail class the command was executed under ("read_only" | "mutating" |
	// "destructive"), or "" when the runner did not report one. It is the ONLY thing that separates
	// "this diagnosis looked at the box" from "this diagnosis changed it", which is the question a
	// user has after an interrupted run — and the adapter was dropping it while the audit row kept
	// it. Every consumer must treat "" as unknown rather than as read_only.
	Tier string
	// Reason names WHICH gate refused, when the runner knows: the destructive tier, the shape gate,
	// a declined/timed-out/disconnected confirmation, or a command too long to put on a card. Empty
	// means unknown, and every consumer must degrade to the old generic wording rather than assume a
	// value.
	Reason   string
	ExitCode *int   // nil for refused/failed commands that never produced an exit status
	Bytes    int    // output byte count (metadata only; the output itself never crosses here)
	JobID    string // opaque background-job handle; never the command that created it
	JobState string // started | running | unknown | succeeded | failed | interrupted | not_found
}

// InstanceOpsVerdict is the terminal root-cause conclusion. Text is the harness's
// already-scrubbed verdict body; Ran/Refused are the command tallies used for the
// summary line.
type InstanceOpsVerdict struct {
	Text    string
	Ran     int
	Refused int
}
