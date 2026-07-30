//go:build live

// Live integration test — NOT compiled in CI (needs the `live` build tag, a reachable
// instance, a running ccr gateway, and the Python harness deps). Run manually:
//
//	go test -tags live -run TestLive -v -timeout 360s ./internal/sshops
//
// Required env (a real, dedicated test box — never commit creds):
//
//	SSHH_HOST SSHH_PORT SSHH_USER SSHH_PASS   — the box
//	SSHH_HARNESS                              — abs path to deploy/ssh_ops_harness/harness.py
//	SSHH_GATEWAY (default http://127.0.0.1:3456)  SSHH_MODEL (default deepseek-v4-flash)
//	SSHH_PYTHON  (default "python")           — interpreter with claude_agent_sdk + paramiko
//	SSHH_INSTANCE (default "uhost-livetest")  SSHH_TASK (default: 掉卡 root-cause probe)
package sshops

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"testing"
	"time"
)

func liveEnv(t *testing.T) (host, port, user, pass string) {
	t.Helper()
	host, port, user, pass = os.Getenv("SSHH_HOST"), os.Getenv("SSHH_PORT"), os.Getenv("SSHH_USER"), os.Getenv("SSHH_PASS")
	if host == "" || user == "" || pass == "" {
		t.Skip("live test needs SSHH_HOST/SSHH_USER/SSHH_PASS")
	}
	if port == "" {
		port = "22"
	}
	return
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// liveDescriber returns a stub DescribeCompShareInstance response carrying the REAL test
// box's SshLoginCommand + base64(password). FetchCredential runs unmodified against it —
// only the upstream HTTP describe is stubbed (it is independently unit-tested), so the whole
// production credential lane (decode + parse + non-serializable Credential) runs for real.
func liveDescriber(host, port, user, pass, instanceID string) Describer {
	login := fmt.Sprintf("ssh -p %s %s@%s", port, user, host)
	if port == "22" {
		login = fmt.Sprintf("ssh %s@%s", user, host)
	}
	return stubDescriber{resp: map[string]any{
		"RetCode": float64(0),
		"UHostSet": []any{map[string]any{
			"UHostId":         instanceID,
			"Name":            "test-3080ti",
			"GpuType":         "RTX3080Ti",
			"GPU":             1,
			"State":           "Running",
			"SshLoginCommand": login,
			"Password":        base64.StdEncoding.EncodeToString([]byte(pass)),
		}},
	}}
}

func liveSupervisor() Supervisor {
	return Supervisor{
		Python:      envOr("SSHH_PYTHON", "python"),
		HarnessPath: os.Getenv("SSHH_HARNESS"),
		GatewayURL:  envOr("SSHH_GATEWAY", "http://127.0.0.1:3456"),
		Model:       envOr("SSHH_MODEL", "deepseek-v4-flash"),
		Timeout:     12 * time.Minute, // sized for the whole command sequence, see Supervisor.Run
		// The write path had no live coverage: every live run selected the read-only prompt because
		// nothing could turn AllowWrites on. Selects the WRITE prompt + write tool
		// description. The ConfirmFunc stays nil, so the mutating tier is still
		// refused: this changes which TEXT the model gets, not what may execute.
		AllowWrites: os.Getenv("SSHH_ALLOW_WRITES") == "1",
	}
}

// TestLiveKeystone proves the full Go->harness->SDK->gateway->flash->SSH chain end to end:
// fetch the credential out-of-band, spawn the real harness, and have flash SSH into the box.
func TestLiveKeystone(t *testing.T) {
	host, port, user, pass := liveEnv(t)
	if os.Getenv("SSHH_HARNESS") == "" {
		t.Skip("need SSHH_HARNESS (abs path to harness.py)")
	}
	instanceID := envOr("SSHH_INSTANCE", "uhost-livetest")
	d := liveDescriber(host, port, user, pass, instanceID)

	cred, err := FetchCredential(context.Background(), d, instanceID)
	if err != nil {
		t.Fatalf("FetchCredential: %v", err)
	}
	t.Logf("resolved target (redacted): %v", cred) // proves the credential never prints its secret

	task := envOr("SSHH_TASK",
		"用户报告这台 GPU 实例\"掉卡\"：nvidia-smi 不可用/检测不到 GPU。请只读排查根因："+
			"先运行 nvidia-smi 看具体报错，再 cat /proc/driver/nvidia/version 与 cat /etc/os-release 核对，"+
			"判断是宿主机驱动问题还是容器内用户态 NVIDIA 驱动/库缺失，给出根因和修复建议。")

	// nil ConfirmFunc on purpose: this test drives the READ-ONLY lane, and a lane with no confirmer
	// is a lane with no human on it, so every mutating command is refused rather than waved through.
	res, err := liveSupervisor().Run(context.Background(), cred, task, func(st Step) {
		t.Logf("[活动流] %s → %s (exit=%v, %d B)", st.Command, st.Disposition, st.ExitCode, st.Bytes)
	}, nil)
	t.Logf("\n========== HARNESS OUTPUT ==========\n%s\n====================================", res.Output)
	if err != nil {
		t.Fatalf("Supervisor.Run: %v (timedOut=%v)", err, res.TimedOut)
	}
	if res.Output == "" {
		t.Fatalf("empty harness output")
	}
	// the plaintext password must never appear in the harness output
	if containsSecret(res.Output, pass) {
		t.Fatalf("SECURITY: password leaked into harness output")
	}
}

// TestLiveFullFlow drives the real Service end to end against a live box. B-only: the model selects
// the tool + UHostId and the engine's authorization card gates consent, so there is no
// symptom-recognition or candidate-list step here — the instance ID comes straight from the caller,
// then Diagnose enters over real SSH and runs the real harness. Run it against a box whose NVIDIA
// user-space driver lib has been relocated to reproduce a "掉卡".
func TestLiveFullFlow(t *testing.T) {
	host, port, user, pass := liveEnv(t)
	if os.Getenv("SSHH_HARNESS") == "" {
		t.Skip("need SSHH_HARNESS (abs path to harness.py)")
	}
	instanceID := envOr("SSHH_INSTANCE", "uhost-livetest")

	d := liveDescriber(host, port, user, pass, instanceID)
	audit := &MemAuditWriter{}
	sup := liveSupervisor()
	// Log the arm. Which system prompt and tool description the harness selects is decided here, and
	// a run whose arm is invisible cannot be compared with another: an A/B was already spent
	// measuring two arms that turned out to be the same one, because SSHH_ALLOW_WRITES was silently
	// not wired at the time and nothing in the output said so.
	t.Logf("[arm] allow_writes=%v model=%s", sup.AllowWrites, sup.Model)
	svc := NewService(sup, audit)

	// The model already selected DiagnoseInstanceInternals{UHostId, Task} and the user authorized it
	// on the engine's card. Enter the instance and diagnose (real harness, real SSH, default 掉卡 probe).
	// SSHH_TASK carries the REAL user phrasing for the scenario under test (the Task the model would
	// have passed). Empty => the harness falls back to its generic read-only health sweep.
	task := os.Getenv("SSHH_TASK")
	t.Logf("[授权后] 进入实例 %s 只读排查 · task=%q", instanceID, task)
	owner := Owner{TopOrganizationID: 1, OrganizationID: 2, RequestUUID: "live-req-1", TurnID: "live-turn-1"}
	res, err := svc.Diagnose(context.Background(), d, owner, instanceID, task, func(st Step) {
		t.Logf("[活动流] %s → %s (exit=%v, %d B)", st.Command, st.Disposition, st.ExitCode, st.Bytes)
	}, nil)
	t.Logf("\n========== [beat 4] 进入实例排查 · 诊断结论 ==========\n%s\n====================================================", res.Output)
	if err != nil {
		t.Fatalf("Diagnose: %v (timedOut=%v)", err, res.TimedOut)
	}
	if containsSecret(res.Output, pass) {
		t.Fatalf("SECURITY: password leaked into verdict")
	}
	if len(audit.Events) != 2 || audit.Events[0].Disposition != "started" {
		t.Fatalf("audit not recorded: %+v", audit.Events)
	}
	t.Logf("[audit] %d 行: begin=%s finish=%s exit=%d bytes=%d (org=%d/%d, instance=%s, 无凭据)",
		len(audit.Events), audit.Events[0].Disposition, audit.Events[1].Disposition,
		audit.Events[1].ExitCode, audit.Events[1].OutputBytes,
		audit.Events[0].TopOrganizationID, audit.Events[0].OrganizationID, audit.Events[0].InstanceID)
}

func containsSecret(s, secret string) bool {
	return secret != "" && len(secret) >= 4 && contains(s, secret)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
