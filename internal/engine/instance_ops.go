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
