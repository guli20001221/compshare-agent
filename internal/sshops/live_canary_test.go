//go:build live

package sshops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/opscontext"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
	"github.com/compshare-agent/internal/zones"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type liveSSHResult struct {
	ExitCode   *int   `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ErrorClass string `json:"error_class"`
}

// liveRunRemote is fixture control for opt-in live canaries, not a product execution path. It uses
// the same reviewed ssh_transport and credential shape as the harness while keeping the password on
// stdin. Tests use it only to create, inspect and remove deterministic fault/acceptance fixtures;
// the operation under test is still performed by the real Agent SDK + MCP lane.
func liveRunRemote(t *testing.T, cred Credential, command string) liveSSHResult {
	t.Helper()
	harnessPath := strings.TrimSpace(os.Getenv("SSHH_HARNESS"))
	if harnessPath == "" {
		t.Fatal("SSHH_HARNESS is required for live fixture control")
	}
	conn, err := json.Marshal(map[string]any{
		"host": cred.Host, "user": cred.User, "port": cred.Port, "password": cred.password,
	})
	require.NoError(t, err)
	request, err := json.Marshal(map[string]string{"command": command})
	require.NoError(t, err)

	const probe = `import json, os, sys
sys.path.insert(0, os.path.dirname(os.environ["SSHH_HARNESS"]))
from ssh_transport import run_ssh
conn = json.loads(sys.stdin.readline())
command = json.loads(sys.stdin.readline())["command"]
print(json.dumps(run_ssh(conn, command), ensure_ascii=False, separators=(",", ":")))`
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, envOr("SSHH_PYTHON", "python"), "-c", probe)
	cmd.Env = os.Environ()
	cmd.Stdin = bytes.NewReader(append(append(conn, '\n'), append(request, '\n')...))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("live SSH fixture transport failed (%T)", err)
	}
	var result liveSSHResult
	if err := json.Unmarshal(bytes.TrimSpace(out), &result); err != nil {
		t.Fatalf("live SSH fixture returned invalid JSON: %v", err)
	}
	return result
}

func requireLiveRemoteOK(t *testing.T, cred Credential, command string) liveSSHResult {
	t.Helper()
	result := liveRunRemote(t, cred, command)
	if result.ExitCode == nil || *result.ExitCode != 0 || result.ErrorClass != "" {
		t.Fatalf("live fixture command failed: exit=%v class=%s stderr=%q",
			result.ExitCode, result.ErrorClass, result.Stderr)
	}
	return result
}

func liveCanarySessionRoot(t *testing.T) string {
	t.Helper()
	base := strings.TrimSpace(os.Getenv("SSHH_SESSION_ROOT"))
	if base == "" {
		t.Fatal("SSHH_SESSION_ROOT is required and must be outside HOME/repository CLAUDE.md ancestors")
	}
	root := filepath.Join(base, "canary-"+uuid.NewString())
	require.NoError(t, os.MkdirAll(root, 0o700))
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

// TestLiveOpsTaskScopeCanary proves the current product authorization contract against a disposable
// test instance: the caller supplies one trusted task-scope grant, the model may perform all
// guest-local recoverable work needed for that task, and no legacy @@CONFIRM round-trip may occur.
// The reasoning-blind destructive/form/control-plane refusals remain inside the harness and are not
// bypassed by this switch. It is opt-in and never selected by the ordinary live suite.
func TestLiveOpsTaskScopeCanary(t *testing.T) {
	if os.Getenv("SSHH_SCOPE_CANARY") != "1" {
		t.Skip("set SSHH_SCOPE_CANARY=1 and run this exact test against a disposable instance")
	}
	instanceID, task := os.Getenv("SSHH_INSTANCE"), os.Getenv("SSHH_TASK")
	if instanceID == "" || task == "" || os.Getenv("SSHH_HARNESS") == "" || os.Getenv("SSHH_API_KEY") == "" {
		t.Fatal("SSHH_INSTANCE/SSHH_TASK/SSHH_HARNESS/SSHH_API_KEY are required")
	}
	top, err := strconv.ParseUint(os.Getenv("SSHH_TOP_ORG"), 10, 32)
	if err != nil {
		t.Fatalf("SSHH_TOP_ORG: %v", err)
	}
	sub, err := strconv.ParseUint(os.Getenv("SSHH_ORG"), 10, 32)
	if err != nil {
		t.Fatalf("SSHH_ORG: %v", err)
	}
	describer, ctx := liveRealDescriber(t)
	audit := &MemAuditWriter{}
	supervisor := liveSupervisor(t)
	if root := os.Getenv("SSHH_SESSION_ROOT"); root != "" {
		supervisor.SessionRoot = root
	}
	modelContext := opscontext.Context{
		SchemaVersion:         opscontext.SchemaVersion,
		RepairScopeAuthorized: true,
		ConversationHistory:   []opscontext.ConversationMessage{{Role: opscontext.ConversationRoleUser, Content: task}},
	}
	if raw := strings.TrimSpace(os.Getenv("SSHH_CONVERSATION_JSON")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &modelContext.ConversationHistory); err != nil {
			t.Fatalf("SSHH_CONVERSATION_JSON: %v", err)
		}
		if len(modelContext.ConversationHistory) == 0 {
			t.Fatal("SSHH_CONVERSATION_JSON must contain at least one role message")
		}
	}
	modelContext.BridgeConversationAnchor = opscontext.ConversationAnchor(modelContext.ConversationHistory)
	requestedAgentSessionID := strings.TrimSpace(os.Getenv("SSHH_AGENT_SESSION_ID"))
	if requestedAgentSessionID != "" {
		if supervisor.SessionRoot == "" {
			t.Fatal("SSHH_AGENT_SESSION_ID requires SSHH_SESSION_ROOT")
		}
		modelContext.AgentSession = &opscontext.AgentSession{
			SessionID:          requestedAgentSessionID,
			WorkdirID:          envOr("SSHH_AGENT_WORKDIR_ID", requestedAgentSessionID),
			Contract:           envOr("SSHH_AGENT_SESSION_CONTRACT", opscontext.AgentSessionContract),
			Model:              supervisor.Model,
			Resume:             os.Getenv("SSHH_AGENT_SESSION_RESUME") == "1",
			ConversationAnchor: strings.TrimSpace(os.Getenv("SSHH_AGENT_SESSION_CONVERSATION_ANCHOR")),
		}
	}
	var compatibilityConfirms atomic.Int32
	var observedAgentSessionID string
	var observedAgentWorkdirID string
	var observedConversationAnchor string
	result, err := NewService(supervisor, audit).DiagnoseWithContext(
		ctx, describer,
		Owner{TopOrganizationID: uint32(top), OrganizationID: uint32(sub),
			RequestUUID: "live-scope-canary", TurnID: fmt.Sprintf("live-scope-%d", time.Now().UnixNano())},
		instanceID, task,
		modelContext,
		func(step Step) {
			if step.AgentSessionLifecycleOnly {
				observedAgentSessionID = step.AgentSessionID
				observedAgentWorkdirID = step.AgentSessionWorkdirID
				observedConversationAnchor = step.AgentSessionConversationAnchor
				t.Logf("agent_session=%s contract=%s model=%s conversation_anchor=%s", step.AgentSessionID,
					step.AgentSessionContract, step.AgentSessionModel, step.AgentSessionConversationAnchor)
				return
			}
			t.Logf("step=%s tier=%s disposition=%s reason=%s", step.Command, step.Tier,
				step.Disposition, step.Reason)
		},
		func(ConfirmRequest) ConfirmDecision {
			compatibilityConfirms.Add(1)
			return ConfirmDecision{Approved: false, TerminalReason: "user_declined"}
		},
	)
	t.Logf("VERDICT:\n%s", result.Output)
	if err != nil {
		t.Fatalf("task-scope canary: %v", err)
	}
	if compatibilityConfirms.Load() != 0 {
		t.Fatalf("task-scope run emitted %d legacy command confirmation(s)", compatibilityConfirms.Load())
	}
	mutatingRan := 0
	for _, step := range result.Steps {
		if step.Tier == "mutating" && step.Disposition == "ran" {
			mutatingRan++
		}
	}
	if mutatingRan == 0 {
		t.Fatal("canary performed no guest mutation, so it did not prove autonomous repair")
	}
	if !result.ContextApplied {
		t.Fatal("task-scope canary did not deliver context to a model turn")
	}
	if requestedAgentSessionID != "" && !modelContext.AgentSession.Resume && observedAgentSessionID != requestedAgentSessionID {
		t.Fatalf("fresh agent session receipt = %q, want %q", observedAgentSessionID, requestedAgentSessionID)
	}
	if requestedAgentSessionID != "" && modelContext.AgentSession.Resume &&
		(observedAgentSessionID == "" || observedAgentSessionID == requestedAgentSessionID) {
		t.Fatalf("resumed agent session did not fork: receipt=%q source=%q", observedAgentSessionID, requestedAgentSessionID)
	}
	if requestedAgentSessionID != "" && observedAgentWorkdirID != modelContext.AgentSession.WorkdirID {
		t.Fatalf("agent workdir receipt = %q, want %q", observedAgentWorkdirID, modelContext.AgentSession.WorkdirID)
	}
	if requestedAgentSessionID != "" && observedConversationAnchor != modelContext.BridgeConversationAnchor {
		t.Fatalf("agent conversation anchor = %q, want %q", observedConversationAnchor,
			modelContext.BridgeConversationAnchor)
	}
	if marker := os.Getenv("SSHH_ASSERT_VERDICT_CONTAINS"); marker != "" && !strings.Contains(result.Output, marker) {
		t.Fatalf("verdict does not contain the requested continuation marker")
	}
}

// TestLiveCase083ConversationActionCanary reproduces the production continuation shape where the
// latest user says only "按上面的来" and the concrete video parameters exist in the prior assistant
// message. Success is the body accepted by a real guest-local HTTP application, not verdict prose.
func TestLiveCase083ConversationActionCanary(t *testing.T) {
	if os.Getenv("SSHH_CONTEXT_ACTION_CANARY") != "1" {
		t.Skip("set SSHH_CONTEXT_ACTION_CANARY=1 and run this exact test against a disposable instance")
	}
	instanceID := strings.TrimSpace(os.Getenv("SSHH_INSTANCE"))
	if instanceID == "" || os.Getenv("SSHH_HARNESS") == "" || os.Getenv("SSHH_API_KEY") == "" {
		t.Fatal("SSHH_INSTANCE/SSHH_HARNESS/SSHH_API_KEY are required")
	}
	top, err := strconv.ParseUint(os.Getenv("SSHH_TOP_ORG"), 10, 32)
	require.NoError(t, err)
	sub, err := strconv.ParseUint(os.Getenv("SSHH_ORG"), 10, 32)
	require.NoError(t, err)
	describer, liveCtx := liveRealDescriber(t)
	cred, err := FetchCredential(liveCtx, describer, instanceID)
	require.NoError(t, err)

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	port := 20000 + int(time.Now().UnixNano()%10000)
	scriptPath := "/tmp/sshops-case083-" + suffix + ".py"
	bodyPath := "/tmp/sshops-case083-" + suffix + ".json"
	attemptsPath := "/tmp/sshops-case083-" + suffix + ".attempts"
	pidPath := "/tmp/sshops-case083-" + suffix + ".pid"
	logPath := "/tmp/sshops-case083-" + suffix + ".log"
	taskID := "case083-accepted-" + suffix
	acceptedHashes := make([]string, 0, 4)
	for duration := 5; duration <= 8; duration++ {
		canonical := fmt.Sprintf(`{"aspect_ratio":"9:16","duration_seconds":%d,"resolution":"720P","shots":[1,2,3,6,8,11]}`, duration)
		sum := sha256.Sum256([]byte(canonical))
		acceptedHashes = append(acceptedHashes, fmt.Sprintf("%x", sum))
	}
	acceptedHashesJSON, err := json.Marshal(acceptedHashes)
	require.NoError(t, err)
	serverTemplate := `import hashlib, json, os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
PORT = __PORT__
BODY = "__BODY__"
ATTEMPTS = "__ATTEMPTS__"
PID = "__PID__"
TASK_ID = "__TASK__"
ACCEPTED_HASHES = set(__ACCEPTED_HASHES__)
SCHEMA = {"method":"POST","path":"/generate","required_fields":{"aspect_ratio":"string","resolution":"string","duration_seconds":"number","shots":"array of integers"}}
class Handler(BaseHTTPRequestHandler):
    def reply(self, code, value):
        payload = json.dumps(value, ensure_ascii=False).encode("utf-8")
        self.send_response(code); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload))); self.end_headers(); self.wfile.write(payload)
    def do_GET(self):
        if self.path == "/health": self.reply(200, {"ok":True})
        elif self.path in ("/", "/schema"): self.reply(200, SCHEMA)
        else: self.reply(404, {"error":"not_found"})
    def do_POST(self):
        if self.path != "/generate": self.reply(404, {"error":"not_found"}); return
        try:
            size = int(self.headers.get("Content-Length", "0"))
            if size < 2 or size > 16384: raise ValueError("invalid body size")
            value = json.loads(self.rfile.read(size))
            with open(ATTEMPTS, "a", encoding="utf-8") as attempts: attempts.write(json.dumps(value, ensure_ascii=False, sort_keys=True) + "\n")
            duration = value.get("duration_seconds")
            if isinstance(duration, float) and duration.is_integer(): value["duration_seconds"] = int(duration)
            resolution = value.get("resolution")
            if isinstance(resolution, str): value["resolution"] = resolution.upper()
            canonical = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8")
            valid = hashlib.sha256(canonical).hexdigest() in ACCEPTED_HASHES
            if not valid: self.reply(422, {"error":"parameters_do_not_match_schema","schema":SCHEMA}); return
            temp = BODY + ".tmp"
            with open(temp, "w", encoding="utf-8") as handle: json.dump(value, handle, ensure_ascii=False, sort_keys=True)
            os.replace(temp, BODY)
            self.reply(202, {"accepted":True,"task_id":TASK_ID})
        except Exception as exc: self.reply(400, {"error":type(exc).__name__})
    def log_message(self, *_args): pass
with open(PID, "w", encoding="ascii") as handle: handle.write(str(os.getpid()))
try: ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
finally:
    try: os.unlink(PID)
    except OSError: pass
`
	serverSource := strings.NewReplacer(
		"__PORT__", strconv.Itoa(port), "__BODY__", bodyPath, "__ATTEMPTS__", attemptsPath,
		"__PID__", pidPath, "__TASK__", taskID, "__ACCEPTED_HASHES__", string(acceptedHashesJSON),
	).Replace(serverTemplate)
	encoded := base64.StdEncoding.EncodeToString([]byte(serverSource))
	requireLiveRemoteOK(t, cred, fmt.Sprintf(
		`python3 -c "import base64;open('%s','wb').write(base64.b64decode('%s'))"`, scriptPath, encoded))
	requireLiveRemoteOK(t, cred, fmt.Sprintf(
		"nohup python3 %s > %s 2>&1 < /dev/null &", scriptPath, logPath))
	t.Cleanup(func() {
		pidResult := liveRunRemote(t, cred, "cat "+pidPath+" 2>/dev/null")
		pid := strings.TrimSpace(pidResult.Stdout)
		if _, parseErr := strconv.Atoi(pid); parseErr == nil {
			_ = liveRunRemote(t, cred, "kill "+pid+" 2>/dev/null; rm -f "+
				scriptPath+" "+bodyPath+" "+bodyPath+".tmp "+attemptsPath+" "+pidPath+" "+logPath)
		} else {
			_ = liveRunRemote(t, cred, "rm -f "+scriptPath+" "+bodyPath+" "+bodyPath+".tmp "+attemptsPath+" "+pidPath+" "+logPath)
		}
	})
	requireLiveRemoteOK(t, cred, fmt.Sprintf(
		`for i in 1 2 3 4 5 6 7 8 9 10; do python3 -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:%d/health',timeout=2).read()" && exit 0; sleep 1; done; exit 1`, port))

	history := []opscontext.ConversationMessage{
		{Role: opscontext.ConversationRoleUser, Content: "我要按刚才讨论的分镜生成一个竖屏短视频。"},
		{Role: opscontext.ConversationRoleAssistant, Content: "已确认使用 9:16，先按 720P，单镜头 5–8 秒；首批生成镜头 1/2/3/6/8/11。"},
		{Role: opscontext.ConversationRoleUser, Content: "直接按上面的来生成视频，你来操作"},
	}
	sessionID := uuid.NewString()
	modelContext := opscontext.Context{
		SchemaVersion:       opscontext.SchemaVersion,
		ConversationHistory: history,
		// The location of an instance-local service is environment evidence, not user intent. The
		// fixture controller has just proved this listener over SSH, so expose that same fact shape
		// the production bridge uses instead of smuggling the port through a lossy planner Task.
		PlatformFacts: []opscontext.Fact{{
			Key: "guest.listeners", Value: map[string]any{"http": []int{port}},
			Source: "live_fixture_ssh", ObservedAt: time.Now().UTC().Format(time.RFC3339),
			Status: opscontext.StatusKnown,
		}},
		BridgeConversationAnchor: opscontext.ConversationAnchor(history),
		RepairScopeAuthorized:    true,
		AgentSession: &opscontext.AgentSession{
			SessionID: sessionID, WorkdirID: sessionID, Contract: opscontext.AgentSessionContract,
		},
	}
	supervisor := liveSupervisor(t)
	supervisor.SessionRoot = liveCanarySessionRoot(t)
	// Reproduce the actual failure, not merely a vague continuation: the planner task carries the
	// stale/conflicting 16:9, 544p, 5-second, 8-step rewrite from production, while the role-complete
	// conversation contains the user's real 9:16, 720P, 5–8 second, selected-shot instruction. The
	// first accepted POST must follow the conversation rather than the lossy planner rewrite.
	task := fmt.Sprintf("在实例内读取 http://127.0.0.1:%d/schema，并按 16:9、544p、5 秒、8 steps 向该本地视频生成验收 API 真正提交一次；取得 task_id 后核对请求已被接受。", port)
	var compatibilityConfirms atomic.Int32
	var receipt Step
	result, err := NewService(supervisor, &MemAuditWriter{}).DiagnoseWithContext(
		liveCtx, describer,
		Owner{TopOrganizationID: uint32(top), OrganizationID: uint32(sub),
			RequestUUID: "live-case083", TurnID: "live-case083-" + suffix},
		instanceID, task, modelContext,
		func(step Step) {
			if step.AgentSessionLifecycleOnly {
				receipt = step
				return
			}
			t.Logf("step=%s tier=%s disposition=%s reason=%s", step.Command, step.Tier, step.Disposition, step.Reason)
		},
		func(ConfirmRequest) ConfirmDecision {
			compatibilityConfirms.Add(1)
			return ConfirmDecision{Approved: false, TerminalReason: "user_declined"}
		},
	)
	t.Logf("VERDICT:\n%s", result.Output)
	require.NoError(t, err)
	require.True(t, result.ContextApplied)
	require.Zero(t, compatibilityConfirms.Load(), "task-scoped lane must not ask for per-command approval")
	require.Equal(t, sessionID, receipt.AgentSessionID)
	require.Equal(t, sessionID, receipt.AgentSessionWorkdirID)
	require.Equal(t, modelContext.BridgeConversationAnchor, receipt.AgentSessionConversationAnchor)

	bodyResult := requireLiveRemoteOK(t, cred, "cat "+bodyPath)
	var submitted struct {
		AspectRatio     string  `json:"aspect_ratio"`
		Resolution      string  `json:"resolution"`
		DurationSeconds float64 `json:"duration_seconds"`
		Shots           []int   `json:"shots"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(bodyResult.Stdout)), &submitted))
	require.Equal(t, "9:16", submitted.AspectRatio)
	require.Equal(t, "720P", strings.ToUpper(submitted.Resolution))
	require.GreaterOrEqual(t, submitted.DurationSeconds, float64(5))
	require.LessOrEqual(t, submitted.DurationSeconds, float64(8))
	require.Equal(t, []int{1, 2, 3, 6, 8, 11}, submitted.Shots)
	attemptsResult := requireLiveRemoteOK(t, cred, "head -n 1 "+attemptsPath)
	var firstSubmitted struct {
		AspectRatio     string  `json:"aspect_ratio"`
		Resolution      string  `json:"resolution"`
		DurationSeconds float64 `json:"duration_seconds"`
		Shots           []int   `json:"shots"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(attemptsResult.Stdout)), &firstSubmitted))
	require.Equal(t, "9:16", firstSubmitted.AspectRatio,
		"the first real submission must use conversation parameters; the API exposes no target values for retry correction")
	require.Equal(t, "720P", strings.ToUpper(firstSubmitted.Resolution))
	require.GreaterOrEqual(t, firstSubmitted.DurationSeconds, float64(5))
	require.LessOrEqual(t, firstSubmitted.DurationSeconds, float64(8))
	require.Equal(t, []int{1, 2, 3, 6, 8, 11}, firstSubmitted.Shots)
}

// TestLiveCase006AbortResumeCanary proves a model turn interrupted after entering the instance can
// continue through an isolated SDK fork while the successful, unpaired outer user row remains the
// antecedent of the next vague complaint.
func TestLiveCase006AbortResumeCanary(t *testing.T) {
	if os.Getenv("SSHH_ABORT_RESUME_CANARY") != "1" {
		t.Skip("set SSHH_ABORT_RESUME_CANARY=1 and run this exact test against a disposable instance")
	}
	instanceID := strings.TrimSpace(os.Getenv("SSHH_INSTANCE"))
	if instanceID == "" || os.Getenv("SSHH_HARNESS") == "" || os.Getenv("SSHH_API_KEY") == "" {
		t.Fatal("SSHH_INSTANCE/SSHH_HARNESS/SSHH_API_KEY are required")
	}
	top, err := strconv.ParseUint(os.Getenv("SSHH_TOP_ORG"), 10, 32)
	require.NoError(t, err)
	sub, err := strconv.ParseUint(os.Getenv("SSHH_ORG"), 10, 32)
	require.NoError(t, err)
	describer, liveCtx := liveRealDescriber(t)
	supervisor := liveSupervisor(t)
	supervisor.SessionRoot = liveCanarySessionRoot(t)
	workdirID := uuid.NewString()
	continuityMarker := "CASE006-UNPAIRED-" + strings.ToUpper(strings.ReplaceAll(uuid.NewString()[:8], "-", ""))
	latestMarker := "CASE006-LATEST-" + strings.ToUpper(strings.ReplaceAll(uuid.NewString()[:8], "-", ""))
	firstHistory := []opscontext.ConversationMessage{{
		Role: opscontext.ConversationRoleUser, Content: instanceID + "\n连续性标记：" + continuityMarker,
	}}
	firstContext := opscontext.Context{
		SchemaVersion:            opscontext.SchemaVersion,
		ConversationHistory:      firstHistory,
		BridgeConversationAnchor: opscontext.ConversationAnchor(firstHistory),
		RepairScopeAuthorized:    true,
		AgentSession: &opscontext.AgentSession{
			SessionID: workdirID, WorkdirID: workdirID, Contract: opscontext.AgentSessionContract,
		},
	}
	firstRunCtx, cancelFirst := context.WithCancel(liveCtx)
	var firstReceipt Step
	settled := 0
	_, firstErr := NewService(supervisor, &MemAuditWriter{}).DiagnoseWithContext(
		firstRunCtx, describer,
		Owner{TopOrganizationID: uint32(top), OrganizationID: uint32(sub),
			RequestUUID: "live-case006-first", TurnID: "live-case006-first-" + strconv.FormatInt(time.Now().UnixNano(), 10)},
		instanceID, "只读检查实例当前系统、GPU、磁盘与监听端口，然后给出简短结论。", firstContext,
		func(step Step) {
			if step.AgentSessionLifecycleOnly {
				firstReceipt = step
				return
			}
			settled++
			cancelFirst()
		},
		func(ConfirmRequest) ConfirmDecision {
			return ConfirmDecision{Approved: false, TerminalReason: "user_declined"}
		},
	)
	cancelFirst()
	require.Error(t, firstErr, "the fixture must end the first outer turn as interrupted")
	require.GreaterOrEqual(t, settled, 1)
	require.NotEmpty(t, firstReceipt.AgentSessionID, "a genuine model event must receipt the resumable transcript before interruption")
	require.Equal(t, workdirID, firstReceipt.AgentSessionWorkdirID)
	require.Equal(t, firstContext.BridgeConversationAnchor, firstReceipt.AgentSessionConversationAnchor)

	fullHistory := append(append([]opscontext.ConversationMessage(nil), firstHistory...),
		opscontext.ConversationMessage{Role: opscontext.ConversationRoleUser,
			Content: "都开始收费还是进不去。请继续排查，并在结论中原样回显上一条和本条消息里的连续性标记。\n本轮连续性标记：" + latestMarker})
	secondContext := opscontext.Context{
		SchemaVersion:            opscontext.SchemaVersion,
		ConversationHistory:      fullHistory,
		BridgeConversationAnchor: opscontext.ConversationAnchor(fullHistory),
		RepairScopeAuthorized:    true,
		AgentSession: &opscontext.AgentSession{
			SessionID: firstReceipt.AgentSessionID, WorkdirID: workdirID,
			Contract: opscontext.AgentSessionContract, Model: supervisor.Model, Resume: true,
			ConversationAnchor: firstReceipt.AgentSessionConversationAnchor,
		},
	}
	var secondReceipt Step
	secondResult, err := NewService(supervisor, &MemAuditWriter{}).DiagnoseWithContext(
		liveCtx, describer,
		Owner{TopOrganizationID: uint32(top), OrganizationID: uint32(sub),
			RequestUUID: "live-case006-second", TurnID: "live-case006-second-" + strconv.FormatInt(time.Now().UnixNano(), 10)},
		instanceID, "继续完成上一轮针对同一实例的排查，并结合最新消息判断仍无法进入的原因。", secondContext,
		func(step Step) {
			if step.AgentSessionLifecycleOnly {
				secondReceipt = step
				return
			}
			t.Logf("resume-step=%s tier=%s disposition=%s reason=%s", step.Command, step.Tier, step.Disposition, step.Reason)
		},
		func(ConfirmRequest) ConfirmDecision {
			return ConfirmDecision{Approved: false, TerminalReason: "user_declined"}
		},
	)
	t.Logf("RESUMED VERDICT:\n%s", secondResult.Output)
	require.NoError(t, err)
	require.True(t, secondResult.ContextApplied)
	require.NotEmpty(t, secondResult.Output)
	require.Contains(t, secondResult.Output, continuityMarker,
		"the unpaired historical user endpoint must remain available after the interrupted turn")
	require.Contains(t, secondResult.Output, latestMarker,
		"the latest outer user suffix must be delivered alongside the resumed SDK transcript")
	require.NotEmpty(t, secondReceipt.AgentSessionID)
	require.NotEqual(t, firstReceipt.AgentSessionID, secondReceipt.AgentSessionID,
		"every resume must commit a successful fork, never append in place")
	require.Equal(t, workdirID, secondReceipt.AgentSessionWorkdirID)
	require.Equal(t, secondContext.BridgeConversationAnchor, secondReceipt.AgentSessionConversationAnchor)
}

// TestLiveCreateOpsCanary creates one explicitly requested, disposable test instance through the
// same catalog/capacity/price/confirmation workflow as production. It is never selected by the
// ordinary `-run TestLive` command: both the exact test name and SSHH_CREATE_CANARY=1 are required.
// The test logs only the resulting instance ID and step names, never upstream response bodies.
//
// Example:
//
//	SSHH_CREATE_CANARY=1 SSHH_CREATE_IMAGE=ComfyUI go test -tags live \
//	  -run '^TestLiveCreateOpsCanary$' -v -timeout 20m ./internal/sshops
func TestLiveCreateOpsCanary(t *testing.T) {
	if os.Getenv("SSHH_CREATE_CANARY") != "1" {
		t.Skip("set SSHH_CREATE_CANARY=1 and run this exact test to create a disposable instance")
	}
	describer, ctx := liveRealDescriber(t)
	top, err := strconv.ParseUint(os.Getenv("SSHH_TOP_ORG"), 10, 32)
	if err != nil {
		t.Fatalf("SSHH_TOP_ORG: %v", err)
	}
	sub, err := strconv.ParseUint(os.Getenv("SSHH_ORG"), 10, 32)
	if err != nil {
		t.Fatalf("SSHH_ORG: %v", err)
	}

	zoneRows, err := zones.FetchSupportZones(ctx, describer, uint32(top), uint32(sub))
	if err != nil {
		t.Fatalf("fetch live zone catalog: %v", err)
	}
	zoneEntries := make([]deployment.ZoneCatalogEntry, 0, len(zoneRows))
	for _, row := range zoneRows {
		zoneEntries = append(zoneEntries, deployment.ZoneCatalogEntry{
			Placement: deployment.ZonePlacement{
				Zone: row.Zone, Region: row.Region, ZoneID: row.ZoneID,
				AzGroup: row.RegionID, IsPod: row.IsPod,
			},
			DisplayName:           row.Describe,
			DisableImageSync:      row.DisableImageSync,
			UnsupportedImageTypes: append([]string(nil), row.UnsupportedImageTypes...),
		})
	}
	if len(zoneEntries) == 0 {
		t.Fatal("live zone catalog is empty")
	}
	if os.Getenv("SSHH_CREATE_RESOLVE_ONLY") == "1" {
		t.Logf("live zone catalog resolved (%d rows); create intentionally not attempted", len(zoneEntries))
		return
	}

	safe := tools.NewSafeToolExecutor(describer, tools.WithMutatingToolsEnabled(true))
	engine := workflow.NewEngine(
		safe.AsToolExecutor(tools.OriginWorkflowInternal),
		func(action string, _ map[string]any) bool {
			if action != "CreateInstanceWorkflow" {
				t.Fatalf("unexpected confirmation action %q", action)
			}
			t.Log("approved disposable canary create after live price/capacity resolution")
			return true
		},
		func(step workflow.StepEvent) {
			t.Logf("step=%s status=%s tool=%s", step.StepName, step.Status, step.Tool)
		},
	)
	imageSource := envOr("SSHH_CREATE_IMAGE_SOURCE", "platform")
	gpuCount, err := strconv.Atoi(envOr("SSHH_CREATE_GPU_COUNT", "1"))
	if err != nil || gpuCount < 1 {
		t.Fatalf("SSHH_CREATE_GPU_COUNT must be a positive integer, got %q", os.Getenv("SSHH_CREATE_GPU_COUNT"))
	}
	params := map[string]any{
		"GpuType":             envOr("SSHH_CREATE_GPU", "4090"),
		"Gpu":                 float64(gpuCount),
		"ChargeType":          envOr("SSHH_CREATE_CHARGE", "Postpay"),
		"ImageSource":         imageSource,
		"ImageName":           envOr("SSHH_CREATE_IMAGE", "ComfyUI"),
		"Name":                fmt.Sprintf("sshops-canary-%d", time.Now().UTC().Unix()),
		"top_organization_id": uint32(top),
		"organization_id":     uint32(sub),
	}
	if zone := os.Getenv("SSHH_CREATE_ZONE"); zone != "" {
		params["Zone"] = zone
	}
	result, err := engine.Run(ctx, workflow.CreateInstanceDef(), params,
		workflow.WithReferenceData(workflow.ReferenceData{
			ZoneCatalog:    deployment.NewZoneCatalogSnapshot(true, zoneEntries),
			ImageSelection: workflow.ImageSelectionUserPinned,
		}))
	if err != nil {
		t.Fatalf("create workflow error: %v", err)
	}
	if !result.Success {
		t.Fatalf("create workflow stopped at %q: %s", result.StoppedAt, result.Message)
	}
	ids, _ := result.Data["UHostIds"].([]any)
	if len(ids) == 0 {
		t.Fatalf("create succeeded without an instance id")
	}
	t.Logf("CREATED_CANARY_INSTANCE=%v", ids[0])
}

// TestLiveTerminateOpsCanary removes one caller-named disposable instance through the same
// tenant-scoped STS executor used by the server. It is the explicit cleanup companion to the create
// canary: the exact test name, switch and instance ID are all required, and no ordinary live run can
// select it accidentally. Only the ID is logged; upstream response bodies and credentials are not.
func TestLiveTerminateOpsCanary(t *testing.T) {
	if os.Getenv("SSHH_TERMINATE_CANARY") != "1" {
		t.Skip("set SSHH_TERMINATE_CANARY=1 and run this exact test to remove a disposable instance")
	}
	instanceID := strings.TrimSpace(os.Getenv("SSHH_INSTANCE"))
	if instanceID == "" || (!strings.HasPrefix(instanceID, "uhost-") && !strings.HasPrefix(instanceID, "cpod-")) {
		t.Fatal("SSHH_INSTANCE must be one explicit uhost-* or cpod-* disposable instance ID")
	}
	describer, ctx := liveRealDescriber(t)
	raw, err := describer.Execute(ctx, "DescribeCompShareInstance", map[string]any{"UHostIds.0": instanceID})
	if err != nil {
		t.Fatalf("describe disposable canary %s before terminate: %v", instanceID, err)
	}
	inst, resolvedID, err := resolveInstance(raw, instanceID)
	if err != nil {
		t.Fatalf("resolve disposable canary %s before terminate: %v", instanceID, err)
	}
	if resolvedID != instanceID {
		t.Fatalf("describe resolved %q, want exact disposable canary %q", resolvedID, instanceID)
	}
	region, _ := inst["Region"].(string)
	zone, _ := inst["Zone"].(string)
	if strings.TrimSpace(region) == "" || strings.TrimSpace(zone) == "" {
		t.Fatalf("describe disposable canary %s returned no region/zone", instanceID)
	}
	if _, err := describer.Execute(ctx, "TerminateCompShareInstance", map[string]any{
		"Region": region, "Zone": zone, "UHostId": instanceID, "ReleaseUDisk": true,
	}); err != nil {
		t.Fatalf("terminate disposable canary %s: %v", instanceID, err)
	}
	t.Logf("TERMINATED_CANARY_INSTANCE=%s", instanceID)
}

// TestLiveOpsWriteCanary is fixture control for an opt-in disposable-instance canary. It executes
// the caller-supplied SSHH_APPROVE_EXACT bytes directly over the reviewed live SSH transport; it
// does not ask a model to reproduce the fault-injection command and does not exercise product
// authorization. TestLiveOpsTaskScopeCanary separately proves that the real Agent SDK + MCP repair
// lane can perform autonomous, scoped guest-local repair without legacy per-command confirmation.
// The explicit test switch plus exact caller command keep this test out of the ordinary live suite.
// The legacy variable name is retained because run_live_sshops.py exposes --approve-exact.
func TestLiveOpsWriteCanary(t *testing.T) {
	if os.Getenv("SSHH_WRITE_CANARY") != "1" {
		t.Skip("set SSHH_WRITE_CANARY=1 and run this exact fixture-control test")
	}
	instanceID := strings.TrimSpace(os.Getenv("SSHH_INSTANCE"))
	exactCommand := os.Getenv("SSHH_APPROVE_EXACT")
	if instanceID == "" || strings.TrimSpace(exactCommand) == "" {
		t.Fatal("SSHH_INSTANCE and SSHH_APPROVE_EXACT (the literal remote shell command) are required")
	}
	if os.Getenv("SSHH_APPROVE_SHELL_EXACT") != "" {
		t.Fatal("SSHH_APPROVE_SHELL_EXACT is obsolete; pass the literal shell command as SSHH_APPROVE_EXACT")
	}
	if os.Getenv("SSHH_HARNESS") == "" {
		t.Fatal("SSHH_HARNESS is required")
	}
	describer, ctx := liveRealDescriber(t)
	cred, err := FetchCredential(ctx, describer, instanceID)
	require.NoError(t, err)
	result := requireLiveRemoteOK(t, cred, exactCommand)
	t.Logf("EXECUTED_EXACT_FIXTURE_COMMAND=%s", exactCommand)
	t.Logf("FIXTURE_RESULT exit=%d stdout_bytes=%d stderr_bytes=%d", *result.ExitCode,
		len(result.Stdout), len(result.Stderr))
}
