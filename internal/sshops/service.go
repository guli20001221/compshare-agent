package sshops

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/compshare-agent/internal/entity"
)

// Owner is the tenant identity carried into the audit row. It is NOT the credential.
type Owner struct {
	TopOrganizationID uint32
	OrganizationID    uint32
	RequestUUID       string
}

// Candidate is one selectable instance for the consent picker. It carries no secret.
type Candidate struct {
	InstanceID string `json:"InstanceId"`
	Name       string `json:"Name"`
	GpuType    string `json:"GpuType"`
	GPU        int    `json:"Gpu"`
	State      string `json:"State"`
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
type harnessRunner interface {
	Run(ctx context.Context, cred Credential, task string) (Result, error)
	RunAPI(ctx context.Context, api APIEndpoint, task string) (Result, error)
}

type Service struct {
	sup   harnessRunner
	audit AuditWriter
}

// NewService wires a Service. sup is normally a sshops.Supervisor value. audit may be nil only in
// tests; production always passes a fail-closed AuditWriter.
func NewService(sup harnessRunner, audit AuditWriter) *Service {
	return &Service{sup: sup, audit: audit}
}

// ListCandidates returns the caller's instances for the consent picker. d is the upstream
// DescribeCompShareInstance executor, already bound to the tenant via ctx (tools.WithUser).
func (s *Service) ListCandidates(ctx context.Context, d Describer) ([]Candidate, error) {
	raw, err := d.Execute(ctx, "DescribeCompShareInstance", map[string]any{"Limit": 100})
	if err != nil {
		return nil, fmt.Errorf("sshops: list instances: %w", err)
	}
	rows := instanceRows(raw)
	out := make([]Candidate, 0, len(rows))
	for _, row := range rows {
		snap := entity.InstanceFromMap(row)
		if snap.UHostId == "" {
			continue
		}
		out = append(out, Candidate{
			InstanceID: snap.UHostId,
			Name:       snap.Name,
			GpuType:    snap.GpuType,
			GPU:        snap.GPU,
			State:      snap.State,
		})
	}
	return out, nil
}

// Diagnose runs ONE consented, read-only in-instance diagnosis. Consent MUST already be verified
// by the caller (the dedicated Action requires Consent==true). Audit is fail-closed: if the start
// record cannot be written, the harness does not run. The returned Result.Output is the harness's
// already-scrubbed verdict; the credential never appears in it.
func (s *Service) Diagnose(ctx context.Context, d Describer, owner Owner, instanceID, task string) (Result, error) {
	if strings.TrimSpace(task) == "" {
		task = DefaultDiagnosisTask
	}
	cred, err := FetchCredential(ctx, d, instanceID)
	if err != nil {
		return Result{}, err // credential-free error (see credential.go)
	}

	ev := AuditEvent{
		RequestUUID:       owner.RequestUUID,
		TopOrganizationID: owner.TopOrganizationID,
		OrganizationID:    owner.OrganizationID,
		InstanceID:        instanceID,
		Task:              task,
		Phase:             "read_only",
	}
	var auditID string
	if s.audit != nil {
		auditID, err = s.audit.Begin(ctx, ev)
		if err != nil {
			// Fail closed: never run an in-instance access we could not record.
			return Result{}, fmt.Errorf("sshops: audit begin failed, refusing to run (fail-closed): %w", err)
		}
	}

	res, runErr := s.sup.Run(ctx, cred, task)

	if s.audit != nil {
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
	}
	return res, runErr
}

// DiagnoseAPI runs ONE API-only diagnosis (destination-B, e.g. billing): it starts a per-task
// loopback api_read proxy scoped to the tenant-bound describer d + the read-only action allowlist,
// spawns the harness in api mode (NO SSH, NO credential), and records the SAME fail-closed audit.
// Consent differs from SSH ops — there is no in-instance access; this is read-only API on the
// caller's own account — but the attempt is still audited. Output is the harness's verdict.
func (s *Service) DiagnoseAPI(ctx context.Context, d Describer, owner Owner, task string, allow []string) (Result, error) {
	if len(allow) == 0 {
		return Result{}, fmt.Errorf("sshops: DiagnoseAPI needs a non-empty action allowlist")
	}
	if strings.TrimSpace(task) == "" {
		return Result{}, fmt.Errorf("sshops: DiagnoseAPI needs a task")
	}
	proxy, err := startAPIProxy(ctx, d, allow)
	if err != nil {
		return Result{}, err
	}
	defer proxy.Close()

	ev := AuditEvent{
		RequestUUID:       owner.RequestUUID,
		TopOrganizationID: owner.TopOrganizationID,
		OrganizationID:    owner.OrganizationID,
		Task:              task,
		Phase:             "api_read",
	}
	var auditID string
	if s.audit != nil {
		auditID, err = s.audit.Begin(ctx, ev)
		if err != nil {
			// Fail closed: never run an access we could not record.
			return Result{}, fmt.Errorf("sshops: audit begin failed, refusing to run (fail-closed): %w", err)
		}
	}

	res, runErr := s.sup.RunAPI(ctx, APIEndpoint{URL: proxy.URL(), Token: proxy.Token(), Actions: allow}, task)

	if s.audit != nil {
		done := ev
		done.ExitCode, done.TimedOut, done.OutputBytes = res.ExitCode, res.TimedOut, len(res.Output)
		done.Disposition = "ok"
		if runErr != nil {
			done.Disposition = "error"
		}
		// Detached from the request ctx so a client disconnect can't orphan the row at "started"
		// (same reasoning as Diagnose — see the WithoutCancel note there).
		_ = s.audit.Finish(context.WithoutCancel(ctx), auditID, done)
	}
	return res, runErr
}

var (
	gpuWord  = regexp.MustCompile(`(?i)gpu|显卡|nvidia|cuda`)
	lostWord = regexp.MustCompile(`(?i)检测不到|找不到|看不见|不可见|不可用|用不了|没反应|消失|没了|不见|报错|command not found|failed to initialize`)
)

// IsInstanceOpsSymptom reports whether the user text describes a GPU-lost / "掉卡" symptom that
// warrants offering an in-instance SSH diagnosis. Deterministic (reasoning-blind), mirroring the
// engine's inferDiagnosisActionFromText GPU patterns — code answers routing, not the model.
func IsInstanceOpsSymptom(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	if strings.Contains(text, "掉卡") || strings.Contains(text, "卡掉") {
		return true
	}
	return gpuWord.MatchString(text) && lostWord.MatchString(text)
}

// instanceRows extracts the instance array from a DescribeCompShareInstance response, tolerating
// the upstream's key variants (mirrors firstInstance in credential.go).
func instanceRows(raw map[string]any) []map[string]any {
	for _, key := range []string{"UHostSet", "UHostInstanceSet", "Instances", "DataSet"} {
		arr, ok := raw[key].([]any)
		if !ok {
			continue
		}
		out := make([]map[string]any, 0, len(arr))
		for _, it := range arr {
			if m, ok := it.(map[string]any); ok {
				out = append(out, m)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}
