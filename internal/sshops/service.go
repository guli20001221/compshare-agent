package sshops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Owner is the tenant identity carried into the audit row. It is NOT the credential.
type Owner struct {
	TopOrganizationID uint32
	OrganizationID    uint32
	RequestUUID       string
	// TurnID is the server-side turn identity used as the audit dedup key (INV-9). On both
	// transport paths it equals the request identity (F21); it is a distinct field so the
	// UNIQUE(turn_id, task_hash) constraint has an explicit, purpose-named source.
	TurnID string
}

// DefaultDiagnosisTask is the read-only "掉卡" root-cause probe used when the caller passes no task.
// It is a natural-language instruction for the harness agent — never a command list — so the
// reasoning-blind guardrails still decide every command the agent proposes.
const DefaultDiagnosisTask = "用户报告这台 GPU 实例\"掉卡\"（nvidia-smi 不可用 / 检测不到 GPU）。" +
	"请用只读命令排查根因：先运行 nvidia-smi 看具体报错，再 cat /proc/driver/nvidia/version 和 " +
	"cat /etc/os-release 核对，判断是宿主机驱动问题，还是容器内用户态 NVIDIA 驱动 / 库（如 libnvidia-ml）" +
	"缺失或版本不匹配。最后给出根因结论和修复建议（修复命令只作为可选步骤写出，不要执行）。"

// Service is the transport-agnostic core of the consent-gated, read-only SSH-ops lane. The HTTP
// Action (and any future WS / CLI entry) calls it AFTER verifying consent. It owns: out-of-band
// credential fetch, the fail-closed audit, and the per-task harness spawn. It never holds, logs,
// or returns the credential — that lives only inside FetchCredential's Credential value and the
// supervisor's one-shot stdin handshake.
// harnessRunner is the harness-spawning surface Diagnose needs. A Supervisor value satisfies it
// (its Run has a value receiver); tests inject a fake so they never spawn the real Python harness.
// onStep fires once per command as it settles, so the caller can surface a LIVE activity stream
// (command + disposition metadata only, never output — INV-6); it may be nil.
type harnessRunner interface {
	Run(ctx context.Context, cred Credential, task string, onStep func(Step)) (Result, error)
}

type Service struct {
	sup   harnessRunner
	audit AuditWriter
}

// NewService wires a Service. sup is normally a sshops.Supervisor value. audit is REQUIRED: Diagnose
// refuses to run (fail-closed) when it is nil, so an in-instance access can never happen unlogged.
// Production passes store.SSHOpsAuditStore; tests pass MemAuditWriter.
func NewService(sup harnessRunner, audit AuditWriter) *Service {
	return &Service{sup: sup, audit: audit}
}

// Diagnose runs ONE consented, read-only in-instance diagnosis. Consent MUST already be verified
// by the caller (the dedicated Action requires Consent==true). Audit is fail-closed: if the start
// record cannot be written, the harness does not run. onStep streams each command's metadata as it
// settles (nil to opt out). The returned Result.Output is the harness's already-scrubbed verdict;
// the credential never appears in it.
func (s *Service) Diagnose(ctx context.Context, d Describer, owner Owner, instanceID, task string, onStep func(Step)) (Result, error) {
	// Fail closed: an in-instance access we could not durably record must never happen. Refuse BEFORE
	// the credential is fetched — no audit sink means no credential pull and no harness spawn. Production
	// always wires a fail-closed AuditWriter (store.SSHOpsAuditStore); a nil audit is a construction /
	// wiring error, not a valid mode, so it is a hard refusal rather than a silent skip.
	if s.audit == nil {
		return Result{}, fmt.Errorf("sshops: no audit writer configured, refusing to run (fail-closed)")
	}
	if strings.TrimSpace(task) == "" {
		task = DefaultDiagnosisTask
	}
	cred, err := FetchCredential(ctx, d, instanceID)
	if err != nil {
		return Result{}, err // credential-free error (see credential.go)
	}
	// INV-13: the box named on the consent card, the box the credential was read from, and the box
	// written to the audit row must be ONE instance. resolveInstance already requires an exact id
	// match (the arr[0] fallback is gone — see credential.go), so this holds structurally; assert it
	// explicitly as fail-closed defense-in-depth against any future regression that reintroduces a
	// mismatch. The audit records the RESOLVED id so "which box did we enter" has one producer.
	if cred.InstanceID != instanceID {
		return Result{}, fmt.Errorf("sshops: resolved instance %q != requested %q, refusing (INV-13)", cred.InstanceID, instanceID)
	}

	ev := AuditEvent{
		RequestUUID:       owner.RequestUUID,
		TurnID:            owner.TurnID,
		TaskHash:          hashTask(task),
		TopOrganizationID: owner.TopOrganizationID,
		OrganizationID:    owner.OrganizationID,
		InstanceID:        cred.InstanceID,
		Task:              task,
		Phase:             "read_only",
	}
	auditID, err := s.audit.Begin(ctx, ev)
	if err != nil {
		// Fail closed: never run an in-instance access we could not record. A UNIQUE(turn_id,
		// task_hash) violation on a durable replay of the same turn lands here too (INV-9).
		return Result{}, fmt.Errorf("sshops: audit begin failed, refusing to run (fail-closed): %w", err)
	}

	res, runErr := s.sup.Run(ctx, cred, task, onStep)

	done := ev
	done.ExitCode, done.TimedOut, done.OutputBytes = res.ExitCode, res.TimedOut, len(res.Output)
	done.Disposition = "ok"
	if runErr != nil {
		done.Disposition = "error"
	}
	// Best effort: the attempt is already durably recorded by Begin; a failed enrichment must
	// not discard a valid verdict, but it is surfaced to logs by the SQL writer's error return.
	// Detached from the request ctx: on client disconnect (browser tab close / curl --max-time)
	// the request ctx is already cancelled, and the SQL writer derives a WithTimeout child from it
	// — a cancelled parent makes the Finish UPDATE fail, orphaning the row forever at "started".
	// WithoutCancel keeps the values but drops the cancellation so the outcome still lands (Go 1.21+).
	_ = s.audit.Finish(context.WithoutCancel(ctx), auditID, done)
	return res, runErr
}

// hashTask is the stable, non-reversible dedup identity of a task. Paired with the turn id it is the
// UNIQUE(turn_id, task_hash) key that refuses a durable replay of the same turn (INV-9). It hashes the
// raw task (the actual replayed input), not the redacted form the audit column stores, so replays of
// one turn collide deterministically; being a hash, it carries nothing sensitive.
func hashTask(task string) string {
	sum := sha256.Sum256([]byte(task))
	return hex.EncodeToString(sum[:])
}
