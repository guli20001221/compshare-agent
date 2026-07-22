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
		Timeout:     6 * time.Minute,
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

	res, err := liveSupervisor().Run(context.Background(), cred, task)
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

// TestLiveFullFlow simulates the whole Option-A flow against the real box, driving the real
// Service: recognition -> list candidates -> consent -> Diagnose (real harness, real SSH). Run it
// against a box whose NVIDIA user-space driver lib has been relocated to reproduce a "掉卡".
func TestLiveFullFlow(t *testing.T) {
	host, port, user, pass := liveEnv(t)
	if os.Getenv("SSHH_HARNESS") == "" {
		t.Skip("need SSHH_HARNESS (abs path to harness.py)")
	}
	instanceID := envOr("SSHH_INSTANCE", "uhost-livetest")

	// Beat 1 — the user asks; code (not the model) recognizes the in-instance ops symptom.
	userMsg := envOr("SSHH_USERMSG", "我怎么掉卡了？")
	if !IsInstanceOpsSymptom(userMsg) {
		t.Fatalf("recognition failed for %q", userMsg)
	}
	t.Logf("[beat 1] 用户: %s   -> 识别为实例内运维症状（掉卡/驱动）", userMsg)

	d := liveDescriber(host, port, user, pass, instanceID)
	audit := &MemAuditWriter{}
	svc := NewService(liveSupervisor(), audit)

	// Beat 2 — list the user's instances for the consent picker.
	cands, err := svc.ListCandidates(context.Background(), d)
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	t.Logf("[beat 2] 列出可选实例：")
	for _, c := range cands {
		t.Logf("        - %s  %q  %dx%s  %s", c.InstanceID, c.Name, c.GPU, c.GpuType, c.State)
	}
	if len(cands) == 0 {
		t.Fatalf("no candidates listed")
	}

	// Beat 3 — the user selects the instance and consents (the dedicated Action requires
	// Consent==true; here we call Diagnose directly, post-consent).
	chosen := cands[0].InstanceID
	t.Logf("[beat 3] 用户选择 %s 并授权只读 SSH 排查", chosen)

	// Beat 4 — enter the instance and diagnose (real harness, real SSH, default 掉卡 probe).
	owner := Owner{TopOrganizationID: 1, OrganizationID: 2, RequestUUID: "live-req-1"}
	res, err := svc.Diagnose(context.Background(), d, owner, chosen, "")
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
