package sshops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strings"

	"github.com/compshare-agent/internal/opscontext"
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

// DefaultDiagnosisTask is the "掉卡" root-cause probe used when the caller passes no task.
// It is a natural-language instruction for the harness agent — never a command list — so the
// reasoning-blind guardrails still decide every command the agent proposes.
const DefaultDiagnosisTask = "用户报告这台 GPU 实例\"掉卡\"（nvidia-smi 不可用 / 检测不到 GPU）。" +
	"请先用只读命令排查根因：运行 nvidia-smi 看具体报错，再 cat /proc/driver/nvidia/version 和 " +
	"cat /etc/os-release 核对，判断是宿主机驱动问题，还是容器内用户态 NVIDIA 驱动 / 库（如 libnvidia-ml）" +
	"缺失或版本不匹配。若根因可在实例内安全修复，发送精确修复命令等待用户逐条确认，执行后验证结果。"

// Service is the transport-agnostic core of the consent-gated SSH-ops repair lane. The HTTP
// Action (and any future entry) calls it AFTER verifying consent. It owns: out-of-band
// credential fetch, the fail-closed audit, and the per-task harness spawn. It never holds, logs,
// or returns the credential — that lives only inside FetchCredential's Credential value and the
// supervisor's one-shot stdin handshake.
// harnessRunner is the harness-spawning surface Diagnose needs. Every runner receives the versioned
// context explicitly; making that part of the required interface prevents a future wrapper from
// silently discarding it and making the audit claim facts reached the model when they did not.
// A Supervisor value satisfies it; tests inject a fake so they never spawn the real Python harness.
// onStep fires for settled commands and the opaque pre-launch background-job handle. The latter is
// marked JobLifecycleOnly so callers retain its opaque cursor without surfacing or auditing
// it as a command; it may be nil.
type harnessRunner interface {
	RunWithContext(ctx context.Context, cred Credential, task string, modelContext opscontext.Context, onStep func(Step), onConfirm ConfirmFunc) (Result, error)
}

type Service struct {
	sup              harnessRunner
	audit            AuditWriter
	hostResolver     HostResolver
	publicIPv6Prefix string
}

// ServiceOption configures addressing for a Service at construction.
type ServiceOption func(*Service)

// WithHostResolver makes the lane dial the address hr chooses instead of the one
// SshLoginCommand advertises. Nil (the default) keeps the advertised address, so a
// deployment that never sets it is byte-identical to before this option existed.
func WithHostResolver(hr HostResolver) ServiceOption {
	return func(s *Service) { s.hostResolver = hr }
}

// WithPublicIPv6Prefix adds a second addressing scheme for the lane to TRY when the internal
// address does not answer: the instance's public IPv4 expressed under a translation prefix.
//
// It does not replace the internal address, it is tried after it, and the public IPv4 itself is
// still never dialled. Empty (the default) disables the whole candidate path — no extra probe,
// no extra log line, and the dial is byte-identical to WithHostResolver alone.
//
// The prefix is deployment network state, so it remains in YAML rather than code.
func WithPublicIPv6Prefix(prefix string) ServiceOption {
	return func(s *Service) { s.publicIPv6Prefix = strings.TrimSpace(prefix) }
}

// NewService wires a Service. sup is normally a sshops.Supervisor value. audit is REQUIRED: Diagnose
// refuses to run (fail-closed) when it is nil, so an in-instance access can never happen unlogged.
// Production passes store.SSHOpsAuditStore; tests pass MemAuditWriter.
func NewService(sup harnessRunner, audit AuditWriter, opts ...ServiceOption) *Service {
	s := &Service{sup: sup, audit: audit}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Diagnose runs ONE consented in-instance diagnosis and confirmation-gated repair. Consent MUST already be verified
// by the caller (the dedicated Action requires Consent==true). Audit is fail-closed: if the start
// record cannot be written, the harness does not run. onStep streams each command's metadata as it
// settles (nil to opt out). The returned Result.Output is the harness's already-scrubbed verdict;
// the credential never appears in it.
func (s *Service) Diagnose(ctx context.Context, d Describer, owner Owner, instanceID, task string, onStep func(Step), onConfirm ConfirmFunc) (Result, error) {
	return s.DiagnoseWithContext(ctx, d, owner, instanceID, task, opscontext.Context{}, onStep, onConfirm)
}

// DiagnoseWithContext is Diagnose with a versioned, independently transported
// reference context for the inner agent. Context is deliberately not appended to
// task: task remains the exact value used for audit hashing and replay identity.
func (s *Service) DiagnoseWithContext(ctx context.Context, d Describer, owner Owner, instanceID, task string, modelContext opscontext.Context, onStep func(Step), onConfirm ConfirmFunc) (Result, error) {
	// Fail closed: an in-instance access we could not durably record must never happen. Refuse BEFORE
	// the credential is fetched — no audit sink means no credential pull and no harness spawn. Production
	// always wires a fail-closed AuditWriter (store.SSHOpsAuditStore); a nil audit is a construction /
	// wiring error, not a valid mode, so it is a hard refusal rather than a silent skip.
	if s.audit == nil {
		return Result{}, fmt.Errorf("sshops: no audit writer configured, refusing to run (fail-closed)")
	}
	// A missing confirmer is not a second product mode. The same repair prompt and tool surface run,
	// while Supervisor answers every confirmation request with Approved=false. That preserves useful
	// diagnosis in direct/live callers without letting a missing UI channel become implicit consent.
	if strings.TrimSpace(task) == "" {
		task = DefaultDiagnosisTask
	}
	cred, inst, err := fetchCredentialWithDialPolicy(ctx, d, instanceID, s.hostResolver,
		dialPolicy{PublicIPv6Prefix: s.publicIPv6Prefix})
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
	modelContext = enrichInstanceOpsContext(ctx, d, modelContext, inst, cred.InstanceID)

	ev := AuditEvent{
		RequestUUID:       owner.RequestUUID,
		TurnID:            owner.TurnID,
		TaskHash:          hashTask(task),
		TopOrganizationID: owner.TopOrganizationID,
		OrganizationID:    owner.OrganizationID,
		InstanceID:        cred.InstanceID,
		Task:              task,
		// The phase is what the box was ENTERED under, so it is taken from the lane's gate rather
		// than from what the harness happened to run: a write-authorized session that ended up
		// issuing only reads still entered under write authority, and the audit has to say so.
		Phase:                "read_write",
		ContextSchemaVersion: modelContext.SchemaVersion,
		ContextFactCoverage:  modelContext.Coverage,
	}
	auditID, err := s.audit.Begin(ctx, ev)
	if err != nil {
		// Fail closed: never run an in-instance access we could not record. A UNIQUE(turn_id,
		// task_hash) violation on a retry of the same turn lands here too (INV-9).
		return Result{}, fmt.Errorf("sshops: audit begin failed, refusing to run (fail-closed): %w", err)
	}

	res, runErr := s.sup.RunWithContext(ctx, cred, task, modelContext, onStep, onConfirm)

	done := ev
	// Begin records the REQUESTED context, before the box is entered. The harness confirms on its wire
	// protocol only once the model turn has actually begun on the prompt carrying that context. If it
	// could not (a bounded prompt fallback, an older harness, or an SDK/transport failure before the
	// first model message), clear the final aggregate fields rather than leaving the finished audit row
	// claiming a delivery that never happened.
	if !res.ContextApplied {
		done.ContextSchemaVersion = 0
		done.ContextFactCoverage = 0
	}
	done.ExitCode, done.TimedOut, done.OutputBytes = res.ExitCode, res.TimedOut, len(res.Output)
	done.CommandsRan, done.CommandsRefused, done.FirstCommandClass = summarizeAuditSteps(res.Steps)
	// The counts alone cannot answer the question a user asks after a disconnect — "did it change
	// anything, and what?" — because a killed run leaves them with no way to tell an approved write
	// that landed from a read that did. res.Steps is populated on the cancel path too (the
	// supervisor accumulates as it parses), so this is the same best-effort Finish carrying more of
	// what it already had.
	done.Steps = summarizeAuditStepDetail(res.Steps)
	done.Disposition = "ok"
	done.ErrClass = res.ErrClass
	switch {
	case runErr != nil:
		done.Disposition = "error"
		// The harness only names a class for a failed DIAL. A supervisor-level failure (timeout,
		// non-zero exit, unparseable stream) has none, so say which it was rather than leaving the
		// column empty and indistinguishable from a clean run.
		if done.ErrClass == "" {
			done.ErrClass = "harness_timeout"
			if !res.TimedOut {
				done.ErrClass = "harness_failed"
			}
		}
	case res.PreflightFailed:
		// The dial never landed: no command ran and Output is a refusal notice, not a diagnosis.
		// The harness exits 0 because this is an orderly refusal rather than a crash.
		done.Disposition = "error"
		if done.ErrClass == "" {
			done.ErrClass = "preflight_failed"
		}
	}
	// Best effort: the attempt is already durably recorded by Begin, so a failed enrichment must not
	// discard a valid verdict — but it must not be silent either. The writer only RETURNS the error;
	// nothing else logs it, so dropping it here would leave a row stuck at "started" with no trace of
	// why. That matters for how the table is read: the started row carries the context schema/coverage
	// that was REQUESTED, and only a finished row (finished_at IS NOT NULL) carries what was applied.
	// Detached from the request ctx: on client disconnect (browser tab close / curl --max-time)
	// the request ctx is already cancelled, and the SQL writer derives a WithTimeout child from it
	// — a cancelled parent makes the Finish UPDATE fail, orphaning the row forever at "started".
	// WithoutCancel keeps the values but drops the cancellation so the outcome still lands (Go 1.21+).
	if err := s.audit.Finish(context.WithoutCancel(ctx), auditID, done); err != nil {
		log.Printf("ssh-ops: audit finish failed for instance %s (row stays 'started'): %v", cred.InstanceID, err)
	}
	return res, runErr
}

// hashTask is the stable, non-reversible dedup identity of a task. Paired with the turn id it is the
// UNIQUE(turn_id, task_hash) key that refuses a retry of the same turn (INV-9). It hashes the
// raw task (the actual replayed input), not the redacted form the audit column stores, so replays of
// one turn collide deterministically; being a hash, it carries nothing sensitive.
func hashTask(task string) string {
	sum := sha256.Sum256([]byte(task))
	return hex.EncodeToString(sum[:])
}
