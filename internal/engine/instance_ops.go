package engine

import (
	"context"
	"errors"
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
// TurnID is the server-side turn identity used as the audit dedup key so a durable
// replay of the same turn cannot re-enter the box (INV-9).
type InstanceOpsRequest struct {
	TurnID     string
	InstanceID string
	Task       string
	// ConfirmWrite asks the user about ONE command that will change the box, and blocks until they
	// answer. It is separate from the lane-level card: that one authorizes entering the instance and
	// never names what will change, so it cannot stand as consent for `kill 6934`. nil means no human
	// is reachable, and the runner must then refuse rather than proceed — see sshops.Service.
	ConfirmWrite func(command string) bool
}

// Progress kinds emitted by a runner. The engine translates each into exactly
// one StepEvent (the live activity stream). The terminal summary line is emitted
// by the engine itself from the verdict tallies, not by the runner.
const (
	// InstanceOpsProgressConnected fires once when the SSH session is established.
	InstanceOpsProgressConnected = "connected"
	// InstanceOpsProgressCommand fires once per command the harness ran or refused.
	InstanceOpsProgressCommand = "command"
)

// InstanceOpsProgress is one activity-stream event. It carries command METADATA
// only — never command output (INV-6) — so it is safe to surface live to the user
// and to persist as an audit row.
type InstanceOpsProgress struct {
	Kind        string // InstanceOpsProgressConnected | InstanceOpsProgressCommand
	Command     string // the command (Kind==command only)
	Disposition string // "ran" | "refused" | "failed" (Kind==command only)
	ExitCode    *int   // nil for refused/failed commands that never produced an exit status
	Bytes       int    // output byte count (metadata only; the output itself never crosses here)
}

// InstanceOpsVerdict is the terminal root-cause conclusion. Text is the harness's
// already-scrubbed verdict body; Ran/Refused are the command tallies used for the
// summary line.
type InstanceOpsVerdict struct {
	Text    string
	Ran     int
	Refused int
}
