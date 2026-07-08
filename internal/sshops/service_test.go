package sshops

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"
)

// fakeRunner stands in for the Supervisor so Diagnose/DiagnoseAPI can be tested without spawning
// the harness.
type fakeRunner struct {
	res      Result
	err      error
	calls    int
	lastCred Credential
	lastAPI  APIEndpoint
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

func (f *fakeRunner) RunAPI(_ context.Context, api APIEndpoint, task string) (Result, error) {
	f.calls++
	f.lastAPI = api
	f.lastTask = task
	if f.onRun != nil {
		f.onRun()
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

// WHY: DiagnoseAPI is the destination-B API-only lane. It must (1) spawn the harness in api mode
// with the read-only allowlist forwarded and NO credential, and (2) record the same fail-closed
// audit under the api_read phase. This asserts the happy path end to end (minus the real proxy hit).
func TestDiagnoseAPIRunsWithAllowlistAndAudit(t *testing.T) {
	d := describerWithInstances(map[string]any{"UHostId": "uhost-a", "Name": "b"})
	runner := &fakeRunner{res: Result{Output: "账单正常", ExitCode: 0}}
	audit := &MemAuditWriter{}
	svc := NewService(runner, audit)
	allow := []string{"DescribeCompShareInstance", "DescribeBilling"}

	res, err := svc.DiagnoseAPI(context.Background(), d,
		Owner{TopOrganizationID: 3, OrganizationID: 4, RequestUUID: "r"}, "查一下这个月账单", allow)
	if err != nil {
		t.Fatalf("DiagnoseAPI: %v", err)
	}
	if res.Output != "账单正常" {
		t.Fatalf("output: %q", res.Output)
	}
	if runner.calls != 1 {
		t.Fatalf("runner calls=%d want 1", runner.calls)
	}
	// api mode: endpoint wired (loopback url + one-time token), allowlist forwarded, no credential.
	if runner.lastAPI.URL == "" || runner.lastAPI.Token == "" || len(runner.lastAPI.Actions) != 2 {
		t.Fatalf("api endpoint not wired: %+v", runner.lastAPI)
	}
	if runner.lastCred.HasSecret() {
		t.Fatalf("api-mode diagnosis must not carry an SSH credential")
	}
	if runner.lastTask != "查一下这个月账单" {
		t.Fatalf("task: %q", runner.lastTask)
	}
	if len(audit.Events) != 2 || audit.Events[0].Disposition != "started" || audit.Events[1].Disposition != "ok" {
		t.Fatalf("audit begin/finish wrong: %+v", audit.Events)
	}
	if audit.Events[0].Phase != "api_read" {
		t.Fatalf("phase not api_read: %+v", audit.Events[0])
	}
}

// WHY: fail-closed applies to the api lane too — no run without a durable record.
func TestDiagnoseAPIFailClosedOnAuditBegin(t *testing.T) {
	d := describerWithInstances(map[string]any{"UHostId": "uhost-a"})
	runner := &fakeRunner{res: Result{Output: "should-not-run"}}
	svc := NewService(runner, &MemAuditWriter{FailBegin: true})

	_, err := svc.DiagnoseAPI(context.Background(), d, Owner{}, "t", []string{"DescribeBilling"})
	if err == nil {
		t.Fatalf("expected fail-closed error when audit Begin fails")
	}
	if runner.calls != 0 {
		t.Fatalf("harness ran despite audit Begin failure (calls=%d) — fail-closed violated", runner.calls)
	}
}

// WHY: deny-by-default starts at the door — an empty allowlist is a misconfiguration, not a
// wide-open proxy. It must be refused before anything spawns.
func TestDiagnoseAPIRejectsEmptyAllowlist(t *testing.T) {
	svc := NewService(&fakeRunner{}, &MemAuditWriter{})
	if _, err := svc.DiagnoseAPI(context.Background(), stubDescriber{}, Owner{}, "t", nil); err == nil {
		t.Fatalf("expected error for empty allowlist")
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
