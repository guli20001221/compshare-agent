//go:build live

// Live integration test — NOT compiled in CI (needs the `live` build tag, a reachable
// instance, a ModelVerse key, and the Python harness deps). Run manually:
//
//	go test -tags live -run TestLive -v -timeout 360s ./internal/sshops
//
// Required env (a real, dedicated test box — never commit creds):
//
//	SSHH_HOST SSHH_PORT SSHH_USER SSHH_PASS   — the box
//	SSHH_HARNESS                              — abs path to deploy/ssh_ops_harness/harness.py
//	SSHH_API_KEY                              — ModelVerse API key
//	SSHH_BASE_URL (default https://api.modelverse.cn)  SSHH_MODEL (default gpt-5.6-terra)
//	SSHH_PYTHON  (default "python")           — interpreter with claude_agent_sdk + paramiko
//	SSHH_INSTANCE (default "uhost-livetest")  SSHH_TASK (default: 掉卡 root-cause probe)
//	SSHH_CONTEXT=0 disables the reference-context arm; any other value enables it (the default).
//	SSHH_CONTEXT_CURRENT_REPORT optionally supplies the raw user wording for the enabled arm.
//
// What these tests do NOT cover, so nobody reads a green live run as broader than it is:
//   - The ordinary TestLiveFullFlow and TestLiveKeystone deny every proposed write. A separate,
//     exact-name TestLiveOpsWriteCanary exists for a dedicated disposable instance; it requires
//     SSHH_WRITE_CANARY=1 + one SSHH_APPROVE_EXACT value and denies every
//     other proposal. It is not selected accidentally by the ordinary live command.
//   - No database. They use MemAuditWriter, so migration 0013's columns, the fail-closed INSERT
//     and the INV-9 UNIQUE(turn_id, task_hash) replay refusal are covered only by unit tests.
//   - No production dial path. There is no internal-IPv6 resolver here, so the address these
//     tests reach is not the address production reaches (see agent.ssh_ops.internal_ipv6).
package sshops

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/config"
	"github.com/compshare-agent/internal/opscontext"
	"github.com/compshare-agent/internal/tools"
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

// liveContextEnabled deliberately has an explicit off switch so one known fault can be run in
// baseline and contextual arms without editing source. TestLiveFullFlow supplies a deny-all exact
// command confirmer, so it exercises the single repair prompt without changing the test box.
func liveContextEnabled() bool { return os.Getenv("SSHH_CONTEXT") != "0" }

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

// liveRealDescriber builds the production ExternalExecutor as the lane's Describer and
// returns the tenant context it must be called with. This is the only way the contextual
// arm exercises the real projection: the stub describer has no image, disks, ports or
// monitor data, so every platform fact would be `unknown` and the A/B would silently
// measure only the prompt fencing.
//
// Identity comes from env, never a default: a live probe pointed at the wrong tenant
// returns an empty instance set, which reads exactly like "the instance is gone".
func liveRealDescriber(t *testing.T) (Describer, context.Context) {
	t.Helper()
	topOrg, org := os.Getenv("SSHH_TOP_ORG"), os.Getenv("SSHH_ORG")
	if topOrg == "" || org == "" {
		t.Skip("SSHH_REAL_DESCRIBE=1 needs SSHH_TOP_ORG and SSHH_ORG")
	}
	cfg, err := config.Load(envOr("SSHH_CONFIG", "../../deploy/conf/config.local.yaml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if project := os.Getenv("SSHH_PROJECT"); project != "" {
		cfg.Agent.ProjectId = project
	}
	if region := os.Getenv("SSHH_REGION"); region != "" {
		cfg.Agent.Region = region
	}
	top, err := strconv.ParseUint(topOrg, 10, 32)
	if err != nil {
		t.Fatalf("SSHH_TOP_ORG: %v", err)
	}
	sub, err := strconv.ParseUint(org, 10, 32)
	if err != nil {
		t.Fatalf("SSHH_ORG: %v", err)
	}
	// Same precedence as the server request path: a configured default role wins,
	// otherwise the per-company template. An empty RoleUrn fails inside the executor
	// with a message that says nothing about which of the two was missing.
	roleURN := strings.TrimSpace(cfg.Agent.STS.DefaultRoleUrn)
	if roleURN == "" {
		roleURN, err = tools.RoleUrnFromTemplate(cfg.Agent.STS.RoleUrnTemplate, uint32(top))
		if err != nil {
			t.Fatalf("role urn: %v", err)
		}
	}
	ctx := tools.WithUser(context.Background(), tools.UserContext{
		TopOrganizationID: uint32(top),
		OrganizationID:    uint32(sub),
		CompanyID:         uint32(top),
		RoleUrn:           roleURN,
		SessionName:       topOrg + "-" + org,
		ProjectId:         cfg.Agent.ProjectId,
		Region:            cfg.Agent.Region,
	})
	return tools.NewExternalExecutor(cfg.Agent), ctx
}

func liveSupervisor() Supervisor {
	return Supervisor{
		Python:        envOr("SSHH_PYTHON", "python"),
		HarnessPath:   os.Getenv("SSHH_HARNESS"),
		BaseURL:       envOr("SSHH_BASE_URL", "https://api.modelverse.cn"),
		APIKey:        os.Getenv("SSHH_API_KEY"),
		Model:         envOr("SSHH_MODEL", "gpt-5.6-terra"),
		PromptVariant: os.Getenv("SSHH_PROMPT_VARIANT"),
		Timeout:       12 * time.Minute, // sized for the whole command sequence, see Supervisor.Run
	}
}

// TestLiveKeystone proves the full Go->harness->SDK->ModelVerse->SSH chain end to end:
// fetch the credential out-of-band, spawn the real harness, and have the model SSH into the box.
func TestLiveKeystone(t *testing.T) {
	host, port, user, pass := liveEnv(t)
	if os.Getenv("SSHH_HARNESS") == "" {
		t.Skip("need SSHH_HARNESS (abs path to harness.py)")
	}
	if os.Getenv("SSHH_API_KEY") == "" {
		t.Skip("need SSHH_API_KEY (ModelVerse API key)")
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

	// nil ConfirmFunc on purpose: this direct harness keystone has no human confirmation channel, so
	// every mutating command is refused rather than waved through.
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
	if len(res.Steps) == 0 {
		t.Fatalf("model produced no remote operation, so this did not prove SDK->MCP->SSH: %s", res.Output)
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
	if os.Getenv("SSHH_HARNESS") == "" {
		t.Skip("need SSHH_HARNESS (abs path to harness.py)")
	}
	if os.Getenv("SSHH_API_KEY") == "" {
		t.Skip("need SSHH_API_KEY (ModelVerse API key)")
	}
	instanceID := envOr("SSHH_INSTANCE", "uhost-livetest")

	// Two describer modes, and the difference decides what the A/B can measure.
	// The stub carries five fields (id/name/gpu/state/login), so a contextual arm
	// run against it gets a user report and a row of `unknown` facts — it can
	// compare fencing, but never whether the FACTS changed the diagnosis. The real
	// executor is the production Describer the server wires, so the projection sees
	// the same image/disks/ports/monitor a real turn would.
	var (
		d    Describer
		ctx  = context.Background()
		pass = os.Getenv("SSHH_PASS")
	)
	if os.Getenv("SSHH_REAL_DESCRIBE") == "1" {
		d, ctx = liveRealDescriber(t)
	} else {
		var host, port, user string
		host, port, user, pass = liveEnv(t)
		d = liveDescriber(host, port, user, pass, instanceID)
	}
	audit := &MemAuditWriter{}
	sup := liveSupervisor()
	// Log the remaining A/B arm. The removed product-level read-only/write split no longer changes
	// the prompt or tool surface; only reference-context delivery is varied here.
	contextEnabled := liveContextEnabled()
	t.Logf("[arm] context=%v model=%s", contextEnabled, sup.Model)
	svc := NewService(sup, audit)

	// The model already selected DiagnoseInstanceInternals{UHostId, Task} and the user authorized it
	// on the engine's card. Enter the instance and diagnose (real harness, real SSH, default 掉卡 probe).
	// SSHH_TASK carries the REAL user phrasing for the scenario under test (the Task the model would
	// have passed). Empty => the harness falls back to its generic diagnose-and-repair task.
	task := os.Getenv("SSHH_TASK")
	t.Logf("[授权后] 进入实例 %s 排查（写操作逐条拒绝）· task=%q", instanceID, task)
	owner := Owner{TopOrganizationID: 1, OrganizationID: 2, RequestUUID: "live-req-1", TurnID: "live-turn-1"}
	modelContext := opscontext.Context{}
	if contextEnabled {
		modelContext.SchemaVersion = opscontext.SchemaVersion
	}
	if contextEnabled {
		if report := envOr("SSHH_CONTEXT_CURRENT_REPORT", task); report != "" {
			modelContext.CurrentUserReport = &opscontext.UserReport{
				Text: report, Source: "live_test.user_report", ObservedAt: opscontext.StatusUnknown, Status: opscontext.StatusReported,
			}
		}
	}
	res, err := svc.DiagnoseWithContext(ctx, d, owner, instanceID, task, modelContext, func(st Step) {
		t.Logf("[活动流] %s → %s (exit=%v, %d B)", st.Command, st.Disposition, st.ExitCode, st.Bytes)
	}, func(request ConfirmRequest) ConfirmDecision {
		t.Logf("[拒绝测试中的写操作] %s", request.Command)
		return ConfirmDecision{Approved: false, TerminalReason: "user_declined"}
	})
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
	if contextEnabled {
		if !res.ContextApplied {
			t.Fatal("context arm did not receive a harness prompt-delivery receipt")
		}
		if audit.Events[1].ContextSchemaVersion != opscontext.SchemaVersion {
			t.Fatalf("finished audit context schema = %d, want %d: %+v", audit.Events[1].ContextSchemaVersion, opscontext.SchemaVersion, audit.Events[1])
		}
	} else if res.ContextApplied || audit.Events[1].ContextSchemaVersion != 0 || audit.Events[1].ContextFactCoverage != 0 {
		t.Fatalf("baseline arm unexpectedly recorded contextual delivery: result=%+v audit=%+v", res, audit.Events[1])
	}
	t.Logf("[audit] %d 行: begin=%s finish=%s exit=%d bytes=%d (org=%d/%d, instance=%s, 无凭据)",
		len(audit.Events), audit.Events[0].Disposition, audit.Events[1].Disposition,
		audit.Events[1].ExitCode, audit.Events[1].OutputBytes,
		audit.Events[0].TopOrganizationID, audit.Events[0].OrganizationID, audit.Events[0].InstanceID)
	t.Logf("[context] requested_schema=%d delivered_schema=%d coverage=%d commands=%d refused=%d first=%s",
		audit.Events[0].ContextSchemaVersion, audit.Events[1].ContextSchemaVersion, audit.Events[1].ContextFactCoverage,
		audit.Events[1].CommandsRan, audit.Events[1].CommandsRefused, audit.Events[1].FirstCommandClass)
}

// TestLiveEndpointTargetInventory is a no-model, no-SSH sanity probe for the private endpoint-target
// projection. It lists only IDs and non-secret labels/counts; raw Software URLs, hosts, tokens and
// credentials are deliberately never logged. Enable explicitly with SSHH_ENDPOINT_INVENTORY=1.
func TestLiveEndpointTargetInventory(t *testing.T) {
	if os.Getenv("SSHH_ENDPOINT_INVENTORY") != "1" {
		t.Skip("set SSHH_ENDPOINT_INVENTORY=1")
	}
	d, ctx := liveRealDescriber(t)
	raw, err := d.Execute(ctx, "DescribeCompShareInstance", map[string]any{})
	if err != nil {
		t.Fatalf("DescribeCompShareInstance: %v", err)
	}
	count := 0
	for _, key := range []string{"UHostSet", "UHostInstanceSet", "Instances", "DataSet"} {
		for _, item := range instanceContextMapSlice(raw[key]) {
			count++
			software, _ := instanceContextDeclaredSoftware(item)
			targets := instanceEndpointTargets(item)
			summaries := make([]string, 0, len(targets))
			for _, target := range targets {
				summaries = append(summaries, target.ID+":"+target.Kind+":"+target.Label)
			}
			t.Logf("instance=%s state=%s gpu=%s declared_software=%v endpoint_targets=%v",
				instanceIDOf(item), allowlistedString(item, "State"), allowlistedString(item, "GpuType"),
				software, summaries)
		}
	}
	if count == 0 {
		t.Fatal("DescribeCompShareInstance returned no instance rows")
	}
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
