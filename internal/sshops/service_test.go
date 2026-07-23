package sshops

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"
)

// fakeRunner stands in for the Supervisor so Diagnose can be tested without spawning the harness.
type fakeRunner struct {
	res      Result
	err      error
	calls    int
	lastCred Credential
	lastTask string
	onRun    func() // optional side effect invoked inside Run (e.g. cancel the request ctx)
}

func (f *fakeRunner) Run(_ context.Context, cred Credential, task string) (Result, error) {
	f.calls++
	f.lastCred = cred
	f.lastTask = task
	if f.onRun != nil {
		f.onRun() // side effect, e.g. cancel the request ctx to simulate a client disconnect mid-run
	}
	return f.res, f.err
}

// ctxCheckAudit records whether the ctx handed to Finish was already cancelled. The real SQL writer
// (store.SSHOpsAuditStore.Finish) derives a WithTimeout child from that ctx, so a cancelled parent
// makes the Finish UPDATE fail and orphans the row at "started".
type ctxCheckAudit struct {
	finishCalled       bool
	finishSawCancelled bool
}

func (a *ctxCheckAudit) Begin(_ context.Context, _ AuditEvent) (string, error) { return "id-1", nil }
func (a *ctxCheckAudit) Finish(ctx context.Context, _ string, _ AuditEvent) error {
	a.finishCalled = true
	a.finishSawCancelled = ctx.Err() != nil
	return nil
}

func describerWithInstances(rows ...map[string]any) Describer {
	arr := make([]any, len(rows))
	for i, r := range rows {
		arr[i] = r
	}
	return stubDescriber{resp: map[string]any{"RetCode": float64(0), "UHostSet": arr}}
}

func TestListCandidates(t *testing.T) {
	d := describerWithInstances(
		map[string]any{"UHostId": "uhost-a", "Name": "box-a", "GpuType": "RTX3080Ti", "GPU": 1, "State": "Running"},
		map[string]any{"UHostId": "uhost-b", "Name": "box-b", "GpuType": "RTX4090", "GPU": 2, "State": "Stopped"},
		map[string]any{"UHostId": "", "Name": "ignored"}, // no id -> skipped
	)
	svc := NewService(&fakeRunner{}, &MemAuditWriter{})
	cands, err := svc.ListCandidates(context.Background(), d)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("want 2 candidates, got %d (%+v)", len(cands), cands)
	}
	if cands[0].InstanceID != "uhost-a" || cands[0].GpuType != "RTX3080Ti" || cands[0].GPU != 1 || cands[0].State != "Running" {
		t.Fatalf("candidate[0] parsed wrong: %+v", cands[0])
	}
}

// WHY: the fail-closed audit is a security invariant — an in-instance access that could not be
// recorded must NOT happen. If a future refactor runs the harness before/around a failed Begin,
// this test fails.
func TestDiagnoseFailClosedOnAuditBegin(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(secretPW))
	d := stubDescriber{resp: describeResp("ssh -p 23 root@10.0.0.9", b64)}
	runner := &fakeRunner{res: Result{Output: "should-not-run"}}
	svc := NewService(runner, &MemAuditWriter{FailBegin: true})

	_, err := svc.Diagnose(context.Background(), d, Owner{TopOrganizationID: 1, OrganizationID: 2}, "uhost-abc", "")
	if err == nil {
		t.Fatalf("expected fail-closed error when audit Begin fails")
	}
	if runner.calls != 0 {
		t.Fatalf("harness ran despite audit Begin failure (calls=%d) — fail-closed violated", runner.calls)
	}
}

// describerFunc adapts a func to the Describer interface so a test can assert the upstream is (or is
// not) consulted.
type describerFunc func(ctx context.Context, action string, args map[string]any) (map[string]any, error)

func (f describerFunc) Execute(ctx context.Context, action string, args map[string]any) (map[string]any, error) {
	return f(ctx, action, args)
}

// WHY: with no audit sink an access can never be recorded, so it must be refused BEFORE the credential
// is even fetched — no audit, no credential pull, no harness. This guards the fail-closed audit
// invariant against a mis-wired (nil-audit) Service reaching a run path. The describer fails the test
// if consulted, proving the refusal precedes the crown-jewel credential fetch.
func TestDiagnoseFailClosedOnNilAudit(t *testing.T) {
	d := describerFunc(func(context.Context, string, map[string]any) (map[string]any, error) {
		t.Fatalf("describer consulted despite nil audit — credential fetched before the fail-closed refusal")
		return nil, nil
	})
	runner := &fakeRunner{res: Result{Output: "should-not-run"}}
	svc := NewService(runner, nil)

	_, err := svc.Diagnose(context.Background(), d, Owner{TopOrganizationID: 1, OrganizationID: 2}, "uhost-abc", "")
	if err == nil {
		t.Fatalf("expected fail-closed error when no audit writer is configured")
	}
	if runner.calls != 0 {
		t.Fatalf("harness ran with no audit sink (calls=%d) — fail-closed violated", runner.calls)
	}
}

func TestDiagnoseRecordsAuditAndDefaultsTask(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(secretPW))
	d := stubDescriber{resp: describeResp("ssh -p 23 root@10.0.0.9", b64)}
	runner := &fakeRunner{res: Result{Output: "健康", ExitCode: 0}}
	audit := &MemAuditWriter{}
	svc := NewService(runner, audit)

	res, err := svc.Diagnose(context.Background(), d, Owner{TopOrganizationID: 7, OrganizationID: 8, RequestUUID: "req-1"}, "uhost-abc", "")
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	if res.Output != "健康" {
		t.Fatalf("unexpected output: %q", res.Output)
	}
	// the credential reached the runner with its secret intact (FetchCredential ran)
	if !runner.lastCred.HasSecret() {
		t.Fatalf("runner did not receive a resolved credential")
	}
	// empty task defaulted to the read-only 掉卡 probe
	if runner.lastTask != DefaultDiagnosisTask {
		t.Fatalf("empty task not defaulted: %q", runner.lastTask)
	}
	// audit: one Begin (started) + one Finish (ok), org-scoped, credential-free
	if len(audit.Events) != 2 {
		t.Fatalf("want 2 audit events (begin+finish), got %d", len(audit.Events))
	}
	if audit.Events[0].Disposition != "started" || audit.Events[1].Disposition != "ok" {
		t.Fatalf("audit dispositions wrong: %q / %q", audit.Events[0].Disposition, audit.Events[1].Disposition)
	}
	if audit.Events[0].TopOrganizationID != 7 || audit.Events[0].OrganizationID != 8 {
		t.Fatalf("audit not org-scoped: %+v", audit.Events[0])
	}
	// the audit event must never carry the password (it has no such field; this guards the type)
	if fmt.Sprintf("%+v", audit.Events[1]) == "" || containsStr(fmt.Sprintf("%+v", audit.Events), secretPW) {
		t.Fatalf("password leaked into audit event")
	}
}

func TestDiagnoseSurfacesRunErrorWithFinish(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(secretPW))
	d := stubDescriber{resp: describeResp("ssh root@10.0.0.9", b64)}
	runner := &fakeRunner{err: fmt.Errorf("boom"), res: Result{TimedOut: true}}
	audit := &MemAuditWriter{}
	svc := NewService(runner, audit)

	_, err := svc.Diagnose(context.Background(), d, Owner{TopOrganizationID: 1, OrganizationID: 1}, "uhost-abc", "custom task")
	if err == nil {
		t.Fatalf("expected run error surfaced")
	}
	if runner.lastTask != "custom task" {
		t.Fatalf("explicit task not honored: %q", runner.lastTask)
	}
	if len(audit.Events) != 2 || audit.Events[1].Disposition != "error" {
		t.Fatalf("error disposition not recorded: %+v", audit.Events)
	}
}

// WHY: on client disconnect (browser tab close / curl --max-time) the request ctx is cancelled
// mid-run. The outcome-recording Finish must run on a ctx detached from that cancellation, or its
// UPDATE fails and the audit row orphans forever at "started" — an access whose disposition can
// never be reconciled. Regression guard for the bug found by the real HTTP+harness E2E.
func TestDiagnoseFinishDetachedFromCancelledRequestCtx(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(secretPW))
	d := stubDescriber{resp: describeResp("ssh -p 23 root@10.0.0.9", b64)}
	ctx, cancel := context.WithCancel(context.Background())
	// Simulate the client disconnecting mid-diagnosis: the request ctx cancels while the harness runs.
	runner := &fakeRunner{res: Result{Output: "partial", TimedOut: true}, err: context.Canceled, onRun: cancel}
	audit := &ctxCheckAudit{}
	svc := NewService(runner, audit)

	_, _ = svc.Diagnose(ctx, d, Owner{TopOrganizationID: 1, OrganizationID: 2}, "uhost-abc", "")
	if !audit.finishCalled {
		t.Fatalf("Finish was not called after a cancelled-mid-run diagnosis")
	}
	if audit.finishSawCancelled {
		t.Fatalf("Finish received a cancelled ctx — the audit UPDATE would fail and orphan the row at 'started'")
	}
}

func TestIsInstanceOpsSymptom(t *testing.T) {
	pos := []string{
		"我怎么掉卡了？", "卡掉了怎么办", "GPU检测不到了", "nvidia-smi 用不了了",
		"显卡不见了", "我的gpu没了", "cuda 报错 failed to initialize", "nvidia-smi command not found",
	}
	neg := []string{
		"", "你好", "帮我创建一个4090实例", "我想找一个便宜的GPU租用", "查询我的余额", "这个模型怎么部署",
	}
	for _, s := range pos {
		if !IsInstanceOpsSymptom(s) {
			t.Errorf("expected ops symptom for %q", s)
		}
	}
	for _, s := range neg {
		if IsInstanceOpsSymptom(s) {
			t.Errorf("did NOT expect ops symptom for %q", s)
		}
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
