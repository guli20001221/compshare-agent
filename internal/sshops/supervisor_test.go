package sshops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/opscontext"
)

const (
	testAnthropicBaseURL = "https://api.modelverse.cn"
	testAnthropicAPIKey  = "modelverse-test-token"
)

// writeFakeHarness writes a tiny python stand-in for harness.py: it reads the stdin handshake,
// confirms (without echoing the password) what it received, dumps its environment key names, then
// optionally sleeps (to exercise abort). No SDK, no SSH, no gateway.
func writeFakeHarness(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "fake_harness.py")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write fake harness: %v", err)
	}
	return p
}

// pythonBin returns a real interpreter, not a zero-byte Windows app-execution
// alias that would launch an installer during tests.
func pythonBin() string {
	for _, c := range []string{"python3", "python"} {
		path, err := exec.LookPath(c)
		if err != nil {
			continue
		}
		if info, statErr := os.Stat(path); statErr == nil && info.Size() == 0 {
			continue // app-execution alias stub — spawning it installs an interpreter
		}
		return path
	}
	return "" // no interpreter that will run; see requirePython
}

// requirePython returns a verified interpreter path or skips the test. It never
// falls back to a bare name that exec.Command would resolve again through PATH.
func requirePython(t *testing.T) string {
	t.Helper()
	if p := pythonBin(); p != "" {
		return p
	}
	t.Skip("no real python interpreter on PATH (every candidate is a zero-byte app-execution " +
		"alias); install one, or put a real interpreter earlier on PATH")
	return ""
}

// TestPythonBinRefusesWhenEveryCandidateIsAnAliasStub pins the case the size check exists for and
// the one a developer box with a real Python cannot reach: EVERY candidate is a stub. Skipping them
// is not enough — what matters is that nothing resolvable is handed back, because exec.Command
// re-resolves a bare name through the same PATH.
func TestPythonBinRefusesWhenEveryCandidateIsAnAliasStub(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"python", "python3"} {
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		// Zero bytes is the shape of a Windows app-execution alias: a reparse stub that reports no
		// content but is still "executable" as far as LookPath is concerned.
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o755); err != nil {
			t.Fatalf("write stub: %v", err)
		}
	}
	t.Setenv("PATH", dir)
	if runtime.GOOS == "windows" {
		t.Setenv("PATHEXT", ".EXE")
	}
	if got := pythonBin(); got != "" {
		t.Fatalf("pythonBin returned %q when every candidate on PATH was a zero-byte alias stub; "+
			"exec.Command resolves that back to the stub and spawning it installs an interpreter", got)
	}
}

const fakeEcho = `
import sys, json, os
line = sys.stdin.readline()
conn = json.loads(line)
print("<<<VERDICT>>>")
print("HANDSHAKE_OK host=%s user=%s port=%s instance=%s task=%s" % (
    conn.get("host"), conn.get("user"), conn.get("port"), conn.get("instance_id"), conn.get("task")))
print("HAS_PASSWORD=%s" % bool(conn.get("password")))
print("AUTH_TOKEN_OK=%s" % (os.environ.get("ANTHROPIC_AUTH_TOKEN") == "modelverse-test-token"))
print("REPAIR_SCOPE_AUTHORIZED=%s" % conn.get("repair_scope_authorized"))
print("AGENT_SESSION=%s/%s/%s/%s" % (
    (conn.get("agent_session") or {}).get("session_id"), (conn.get("agent_session") or {}).get("contract"),
    (conn.get("agent_session") or {}).get("model"), (conn.get("agent_session") or {}).get("resume")))
print("AGENT_ATTEMPT=%s" % (conn.get("agent_session") or {}).get("attempt_session_id"))
print("AGENT_WORKDIR=%s" % (conn.get("agent_session") or {}).get("workdir_id"))
print("SESSION_ROOT=%s" % conn.get("session_root"))
print("CONVERSATION=" + json.dumps((conn.get("context") or {}).get("conversation_history"), ensure_ascii=False, separators=(",", ":")))
print("CONVERSATION_ANCHOR=%s" % conn.get("conversation_anchor"))
print("CONVERSATION_RESUME_INDEX=%s" % conn.get("conversation_resume_index"))
print("ENVKEYS=" + ",".join(sorted(os.environ.keys())))
print("<<<END>>>")
`

const fakeAgentFailureOutcome = `
import sys, json
json.loads(sys.stdin.readline())
print('@@OUTCOME {"outcome":"agent_failed","err_class":"server_error","context_applied":true}')
print("<<<VERDICT>>>")
print("诊断中断：实例内诊断代理未能完成本轮，因此没有形成经验证的最终结论。")
print("<<<END>>>")
`

// This exercises the process boundary, not only parseHarnessStream in isolation. If RunWithContext
// ever forgets to project a parsed @@OUTCOME into Result, the service would audit a provider failure
// as a successful diagnosis even though both parser-only and fake-runner service tests stayed green.
func TestSupervisorProjectsAgentFailureOutcomeIntoResult(t *testing.T) {
	sup := Supervisor{
		Python:      requirePython(t),
		HarnessPath: writeFakeHarness(t, fakeAgentFailureOutcome),
		SessionRoot: t.TempDir(),
		BaseURL:     testAnthropicBaseURL,
		APIKey:      testAnthropicAPIKey,
		Model:       "gpt-5.6-terra",
		Timeout:     30 * time.Second,
	}
	res, err := sup.RunWithContext(context.Background(),
		cred("uhost-abc", "1.2.3.4", "root", 22, "pw"), "diagnose", opscontext.Context{}, nil, nil)
	if err != nil {
		t.Fatalf("run: %v (output=%q)", err, res.Output)
	}
	if !res.AgentFailed || res.PreflightFailed || res.ErrClass != "server_error" || !res.ContextApplied {
		t.Fatalf("result did not preserve the harness failure receipt: %+v", res)
	}
	if strings.Contains(res.Output, "server_error") || !strings.Contains(res.Output, "没有形成经验证的最终结论") {
		t.Fatalf("customer verdict leaked wire metadata or lost the bounded failure message: %q", res.Output)
	}
}

func TestSupervisorHandshakeAndScrubbedEnv(t *testing.T) {
	// a secret in the PARENT env that must NOT reach the child (proves env scrubbing)
	os.Setenv("LLM_API_KEY", "parent-secret-should-not-leak")
	os.Setenv("MYSQL_DSN", "user:pw@tcp/db")
	defer os.Unsetenv("LLM_API_KEY")
	defer os.Unsetenv("MYSQL_DSN")

	sup := Supervisor{
		Python:      requirePython(t),
		HarnessPath: writeFakeHarness(t, fakeEcho),
		SessionRoot: "/private/sshops-sessions",
		BaseURL:     testAnthropicBaseURL,
		APIKey:      testAnthropicAPIKey,
		Model:       "gpt-5.6-terra",
		Timeout:     30 * time.Second,
	}
	c := cred("uhost-abc", "1.2.3.4", "root", 23, "S3cr3tPw")

	modelContext := opscontext.Context{
		RepairScopeAuthorized: true,
		AgentSession: &opscontext.AgentSession{
			SessionID: "11111111-1111-4111-8111-111111111111", Contract: "sshops-agent-v1",
			WorkdirID: "22222222-2222-4222-8222-222222222222",
			Model:     "gpt-5.6-terra", Resume: true,
		},
	}
	res, err := sup.RunWithContext(context.Background(), c, "health check", modelContext, nil, nil)
	if err != nil {
		t.Fatalf("run: %v (output=%q)", err, res.Output)
	}
	out := res.Output

	// the handshake was delivered (non-secret fields visible to the child)
	if !strings.Contains(out, "host=1.2.3.4 user=root port=23 instance=uhost-abc") {
		t.Fatalf("handshake not delivered: %q", out)
	}
	if !strings.Contains(out, "task=health check") {
		t.Fatalf("task not delivered over stdin: %q", out)
	}
	if !strings.Contains(out, "HAS_PASSWORD=True") {
		t.Fatalf("password not delivered over stdin: %q", out)
	}
	// INV: the password is never echoed back in the output
	if strings.Contains(out, "S3cr3tPw") {
		t.Fatalf("password leaked into harness output: %q", out)
	}
	// INV-3: the server's secret env vars were scrubbed from the child env
	if strings.Contains(out, "LLM_API_KEY") || strings.Contains(out, "MYSQL_DSN") {
		t.Fatalf("parent secret env leaked into child: %q", out)
	}
	// The child gets only the dedicated Anthropic endpoint/token it needs, not the parent key names.
	if !strings.Contains(out, "ANTHROPIC_BASE_URL") || !strings.Contains(out, "AUTH_TOKEN_OK=True") {
		t.Fatalf("ModelVerse Anthropic config not passed: %q", out)
	}
	if strings.Contains(out, "ANTHROPIC_API_KEY") {
		t.Fatalf("legacy/dummy Anthropic API key should not be present: %q", out)
	}
	if !strings.Contains(out, "REPAIR_SCOPE_AUTHORIZED=True") ||
		!strings.Contains(out, "AGENT_SESSION=11111111-1111-4111-8111-111111111111/sshops-agent-v1/gpt-5.6-terra/True") ||
		!strings.Contains(out, "SESSION_ROOT=/private/sshops-sessions") {
		t.Fatalf("private authorization/session handshake fields were not delivered: %q", out)
	}
}

func TestSupervisorResumeKeepsFreshFallbackAndMarksTheOuterConversationDelta(t *testing.T) {
	full := []opscontext.ConversationMessage{
		{Role: opscontext.ConversationRoleUser, Content: "第一轮"},
		{Role: opscontext.ConversationRoleAssistant, Content: "使用 9:16、720p、5–8 秒"},
		{Role: opscontext.ConversationRoleUser, Content: "直接按上面的来"},
	}
	bridged := full[:1]
	sup := Supervisor{
		Python: requirePython(t), HarnessPath: writeFakeHarness(t, fakeEcho),
		SessionRoot: "/private/sshops-sessions", BaseURL: testAnthropicBaseURL,
		APIKey: testAnthropicAPIKey, Model: "gpt-5.6-terra", Timeout: 30 * time.Second,
	}
	ctx := opscontext.Context{
		SchemaVersion:       opscontext.SchemaVersion,
		ConversationHistory: full,
		AgentSession: &opscontext.AgentSession{
			SessionID: "11111111-1111-4111-8111-111111111111", Contract: opscontext.AgentSessionContract,
			WorkdirID: "22222222-2222-4222-8222-222222222222",
			Model:     "gpt-5.6-terra", Resume: true,
			ConversationAnchor: opscontext.ConversationAnchor(bridged),
		},
		BridgeConversationAnchor: opscontext.ConversationAnchor(full),
	}
	res, err := sup.RunWithContext(context.Background(), cred("uhost-abc", "1.2.3.4", "root", 22, "pw"),
		"task", ctx, nil, nil)
	if err != nil {
		t.Fatalf("run: %v (output=%q)", err, res.Output)
	}
	requireContains := func(value string) {
		t.Helper()
		if !strings.Contains(res.Output, value) {
			t.Fatalf("output missing %q: %s", value, res.Output)
		}
	}
	requireContains(`CONVERSATION=[{"role":"user","content":"第一轮"},{"role":"assistant","content":"使用 9:16、720p、5–8 秒"},{"role":"user","content":"直接按上面的来"}]`)
	requireContains("CONVERSATION_ANCHOR=" + opscontext.ConversationAnchor(full))
	requireContains("CONVERSATION_RESUME_INDEX=1")
	requireContains("AGENT_WORKDIR=22222222-2222-4222-8222-222222222222")
	attempt := regexp.MustCompile(`AGENT_ATTEMPT=([0-9a-f-]{36})`).FindStringSubmatch(res.Output)
	if len(attempt) != 2 || attempt[1] == "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("resume was not isolated into a distinct attempt session: %s", res.Output)
	}
	retry, err := sup.RunWithContext(context.Background(), cred("uhost-abc", "1.2.3.4", "root", 22, "pw"),
		"task", ctx, nil, nil)
	if err != nil {
		t.Fatalf("retry run: %v (output=%q)", err, retry.Output)
	}
	retryAttempt := regexp.MustCompile(`AGENT_ATTEMPT=([0-9a-f-]{36})`).FindStringSubmatch(retry.Output)
	if len(retryAttempt) != 2 || retryAttempt[1] == attempt[1] {
		t.Fatalf("a retry reused the prior uncommitted attempt: first=%v retry=%v", attempt, retryAttempt)
	}
	if ctx.AgentSession.AttemptSessionID != "" {
		t.Fatalf("supervisor mutated the engine-owned committed cursor: %+v", ctx.AgentSession)
	}
}

func TestSupervisorFreshSessionSendsTheCompleteOuterConversationOnce(t *testing.T) {
	full := []opscontext.ConversationMessage{
		{Role: opscontext.ConversationRoleUser, Content: "问题"},
		{Role: opscontext.ConversationRoleAssistant, Content: "上轮方案"},
		{Role: opscontext.ConversationRoleUser, Content: "就这个"},
	}
	sup := Supervisor{
		Python: requirePython(t), HarnessPath: writeFakeHarness(t, fakeEcho),
		SessionRoot: "/private/sshops-sessions", BaseURL: testAnthropicBaseURL,
		APIKey: testAnthropicAPIKey, Model: "gpt-5.6-terra", Timeout: 30 * time.Second,
	}
	ctx := opscontext.Context{
		SchemaVersion:       opscontext.SchemaVersion,
		ConversationHistory: full,
		AgentSession: &opscontext.AgentSession{
			SessionID: "11111111-1111-4111-8111-111111111111", Contract: opscontext.AgentSessionContract,
			WorkdirID: "11111111-1111-4111-8111-111111111111",
			Resume:    false,
		},
		BridgeConversationAnchor: opscontext.ConversationAnchor(full),
	}
	res, err := sup.RunWithContext(context.Background(), cred("uhost-abc", "1.2.3.4", "root", 22, "pw"),
		"task", ctx, nil, nil)
	if err != nil {
		t.Fatalf("run: %v (output=%q)", err, res.Output)
	}
	for _, content := range []string{`"content":"问题"`, `"content":"上轮方案"`, `"content":"就这个"`} {
		if strings.Count(res.Output, content) != 1 {
			t.Fatalf("fresh handshake did not carry each outer message exactly once (%s): %s", content, res.Output)
		}
	}
	if !strings.Contains(res.Output, "CONVERSATION_RESUME_INDEX=0") {
		t.Fatalf("fresh session unexpectedly carried a resume suffix: %s", res.Output)
	}
}

func TestSupervisorAnchorMissStartsFreshWithCompleteSnapshot(t *testing.T) {
	full := []opscontext.ConversationMessage{{Role: opscontext.ConversationRoleUser, Content: "bounded current snapshot"}}
	sup := Supervisor{
		Python: requirePython(t), HarnessPath: writeFakeHarness(t, fakeEcho),
		SessionRoot: "/private/sshops-sessions", BaseURL: testAnthropicBaseURL,
		APIKey: testAnthropicAPIKey, Model: "gpt-5.6-terra", Timeout: 30 * time.Second,
	}
	oldID := "11111111-1111-4111-8111-111111111111"
	ctx := opscontext.Context{
		SchemaVersion:       opscontext.SchemaVersion,
		ConversationHistory: full,
		AgentSession: &opscontext.AgentSession{
			SessionID: oldID, Contract: opscontext.AgentSessionContract, Model: "gpt-5.6-terra", Resume: true,
			WorkdirID:          oldID,
			ConversationAnchor: opscontext.ConversationAnchor([]opscontext.ConversationMessage{{Role: "user", Content: "dropped prefix"}}),
		},
		BridgeConversationAnchor: opscontext.ConversationAnchor(full),
	}
	res, err := sup.RunWithContext(context.Background(), cred("uhost-abc", "1.2.3.4", "root", 22, "pw"),
		"task", ctx, nil, nil)
	if err != nil {
		t.Fatalf("run: %v (output=%q)", err, res.Output)
	}
	if strings.Contains(res.Output, oldID) || !strings.Contains(res.Output, "/"+opscontext.AgentSessionContract+"/gpt-5.6-terra/False") {
		t.Fatalf("anchor miss did not rotate to a fresh session: %s", res.Output)
	}
	if !strings.Contains(res.Output, `CONVERSATION=[{"role":"user","content":"bounded current snapshot"}]`) {
		t.Fatalf("fresh fallback did not receive the complete snapshot: %s", res.Output)
	}
	if !strings.Contains(res.Output, "CONVERSATION_RESUME_INDEX=0") {
		t.Fatalf("fresh fallback carried a stale resume index: %s", res.Output)
	}
}

func TestSupervisorEmptySessionRootDisablesCursorAndUsesHarnessCleanWorkdir(t *testing.T) {
	sup := Supervisor{
		Python: requirePython(t), HarnessPath: writeFakeHarness(t, fakeEcho),
		SessionRoot: "  ", BaseURL: testAnthropicBaseURL,
		APIKey: testAnthropicAPIKey, Model: "gpt-5.6-terra", Timeout: 30 * time.Second,
	}
	session := &opscontext.AgentSession{
		SessionID: "11111111-1111-4111-8111-111111111111", Contract: "sshops-agent-v1",
		WorkdirID: "11111111-1111-4111-8111-111111111111",
		Model:     "gpt-5.6-terra", Resume: true,
	}
	res, err := sup.RunWithContext(context.Background(), cred("uhost-abc", "1.2.3.4", "root", 22, "pw"),
		"task", opscontext.Context{AgentSession: session}, nil, nil)
	if err != nil {
		t.Fatalf("run: %v (output=%q)", err, res.Output)
	}
	if !strings.Contains(res.Output, "AGENT_SESSION=None/None/None/None") ||
		!strings.Contains(res.Output, "SESSION_ROOT=") {
		t.Fatalf("empty session root must omit the private resume cursor: %q", res.Output)
	}
	if strings.Contains(res.Output, session.SessionID) {
		t.Fatalf("persisted cursor escaped without a configured stable root: %q", res.Output)
	}
}

func TestSupervisorBindsFreshAgentSessionToSelectedModel(t *testing.T) {
	sup := Supervisor{
		Python: requirePython(t), HarnessPath: writeFakeHarness(t, fakeEcho),
		SessionRoot: "/private/sshops-sessions", BaseURL: testAnthropicBaseURL,
		APIKey: testAnthropicAPIKey, Model: "gpt-5.6-terra", Timeout: 30 * time.Second,
	}
	session := &opscontext.AgentSession{
		SessionID: "11111111-1111-4111-8111-111111111111", Contract: "sshops-agent-v1",
		WorkdirID: "11111111-1111-4111-8111-111111111111",
	}
	res, err := sup.RunWithContext(context.Background(), cred("uhost-abc", "1.2.3.4", "root", 22, "pw"),
		"task", opscontext.Context{AgentSession: session}, nil, nil)
	if err != nil {
		t.Fatalf("run: %v (output=%q)", err, res.Output)
	}
	if !strings.Contains(res.Output,
		"AGENT_SESSION=11111111-1111-4111-8111-111111111111/sshops-agent-v1/gpt-5.6-terra/False") {
		t.Fatalf("fresh cursor was not bound to the selected deployment model: %q", res.Output)
	}
	if session.Model != "" {
		t.Fatalf("supervisor mutated caller-owned private context: %+v", session)
	}
}

func TestSupervisorRotatesResumeCursorWhenDeploymentModelChanges(t *testing.T) {
	sup := Supervisor{
		Python: requirePython(t), HarnessPath: writeFakeHarness(t, fakeEcho),
		SessionRoot: "/private/sshops-sessions", BaseURL: testAnthropicBaseURL,
		APIKey: testAnthropicAPIKey, Model: "gpt-5.6-terra", Timeout: 30 * time.Second,
	}
	session := &opscontext.AgentSession{
		SessionID: "11111111-1111-4111-8111-111111111111", Contract: "sshops-agent-v1",
		WorkdirID: "11111111-1111-4111-8111-111111111111",
		Model:     "old-deployment-model", Resume: true,
	}
	res, err := sup.RunWithContext(context.Background(), cred("uhost-abc", "1.2.3.4", "root", 22, "pw"),
		"task", opscontext.Context{AgentSession: session}, nil, nil)
	if err != nil {
		t.Fatalf("run: %v (output=%q)", err, res.Output)
	}
	if strings.Contains(res.Output, session.SessionID) ||
		!strings.Contains(res.Output, "/sshops-agent-v1/gpt-5.6-terra/False") {
		t.Fatalf("stale model cursor was not rotated to a fresh selected-model session: %q", res.Output)
	}
	if session.Model != "old-deployment-model" || !session.Resume {
		t.Fatalf("supervisor mutated caller-owned resume cursor: %+v", session)
	}
}

func TestSupervisorCredentialAndTaskNotInArgv(t *testing.T) {
	// The fake echoes its own argv into the verdict block. Neither the credential nor the task may be
	// there: the credential never leaves stdin, and the task moved onto the stdin handshake too (it can
	// carry PII, and argv is visible to `ps` on the host).
	fake := "import sys,json; json.loads(sys.stdin.readline()); " +
		"print('<<<VERDICT>>>'); print('ARGV=' + ' '.join(sys.argv[1:])); print('<<<END>>>')"
	sup := Supervisor{
		Python:      requirePython(t),
		HarnessPath: writeFakeHarness(t, fake),
		BaseURL:     testAnthropicBaseURL,
		APIKey:      testAnthropicAPIKey,
		Timeout:     30 * time.Second,
	}
	c := cred("uhost-abc", "h", "root", 22, "ArgvMustNotHaveThis")
	res, err := sup.Run(context.Background(), c, "diagnose gpu memory", nil, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(res.Output, "ARGV=") {
		t.Fatalf("fake did not report its argv: %q", res.Output)
	}
	if strings.Contains(res.Output, "ArgvMustNotHaveThis") {
		t.Fatalf("password surfaced in argv/output: %q", res.Output)
	}
	if strings.Contains(res.Output, "diagnose gpu memory") {
		t.Fatalf("task leaked into argv (must ride the stdin handshake): %q", res.Output)
	}
}

func TestSupervisorAbortKillsProcess(t *testing.T) {
	sup := Supervisor{
		Python:      requirePython(t),
		HarnessPath: writeFakeHarness(t, "import sys,time; sys.stdin.readline(); time.sleep(60); print('SHOULD_NOT_PRINT')"),
		BaseURL:     testAnthropicBaseURL,
		APIKey:      testAnthropicAPIKey,
		Timeout:     1 * time.Second, // hard timeout kills the sleeping child
	}
	c := cred("x", "h", "root", 22, "pw")

	start := time.Now()
	res, err := sup.Run(context.Background(), c, "task", nil, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected timeout error")
	}
	if !res.TimedOut {
		t.Fatalf("expected TimedOut=true")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("abort did not kill promptly: took %s", elapsed)
	}
	if strings.Contains(res.Output, "SHOULD_NOT_PRINT") {
		t.Fatalf("process completed instead of being killed")
	}
}

func TestSupervisorRequiresSecretAndPath(t *testing.T) {
	if _, err := (Supervisor{HarnessPath: "x", BaseURL: testAnthropicBaseURL, APIKey: testAnthropicAPIKey}).Run(context.Background(),
		Credential{Host: "h", User: "u", Port: 22}, "t", nil, nil); err == nil {
		t.Fatalf("expected error for credential without secret")
	}
	if _, err := (Supervisor{}).Run(context.Background(),
		Credential{Host: "h", User: "u", Port: 22, password: "p"}, "t", nil, nil); err == nil {
		t.Fatalf("expected error for missing harness path")
	}
	if _, err := (Supervisor{HarnessPath: "x", APIKey: testAnthropicAPIKey}).Run(context.Background(),
		Credential{Host: "h", User: "u", Port: 22, password: "p"}, "t", nil, nil); err == nil {
		t.Fatalf("expected error for missing Anthropic base URL")
	}
	if _, err := (Supervisor{HarnessPath: "x", BaseURL: testAnthropicBaseURL}).Run(context.Background(),
		Credential{Host: "h", User: "u", Port: 22, password: "p"}, "t", nil, nil); err == nil {
		t.Fatalf("expected error for missing Anthropic API key")
	}
}

func TestParseHarnessStream(t *testing.T) {
	in := strings.Join([]string{
		"claude cli starting...", // chatter -> ignored
		`@@AGENT_SESSION {"session_id":"11111111-1111-4111-8111-111111111111","contract":"sshops-agent-v1","model":"gpt-5.6-terra"}`,
		`@@AGENT_SESSION {"session_id":"22222222-2222-4222-8222-222222222222","contract":"sshops-agent-v1","model":"must be ignored"}`,
		`@@JOB {"job_id":"job-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","job_state":"unknown","purpose":"download model weights"}`,
		`@@JOB {"job_id":"job-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","job_state":"unknown","purpose":"duplicate must be ignored"}`,
		`@@STEP {"command":"poll_background_job","tier":"read_only","disposition":"ran","exit":0,"bytes":42,"job_id":"job-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","job_state":"running","purpose":"download model weights"}`,
		`@@JOB {"job_id":"job-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","job_state":"unknown","purpose":"compile runtime"}`,
		`@@STEP {"command":"rm -rf /","tier":"destructive","disposition":"refused","exit":null,"bytes":0}`,
		"more chatter",
		"<<<VERDICT>>>",
		"GPU 健康。",
		"显存 512MiB 已用。",
		"<<<END>>>",
	}, "\n") + "\n"

	var streamed []Step
	verdict, steps, _, err := parseHarnessStream(strings.NewReader(in), func(st Step) { streamed = append(streamed, st) }, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if verdict != "GPU 健康。\n显存 512MiB 已用。" {
		t.Fatalf("verdict body wrong: %q", verdict)
	}
	if len(steps) != 2 {
		t.Fatalf("want 2 steps, got %d (%+v)", len(steps), steps)
	}
	// The opaque pre-launch handle is live-only: it reaches onStep first but is not returned as a
	// command/audit step. Then each @@STEP streams in order and matches the returned Steps.
	if len(streamed) != 5 || !streamed[0].AgentSessionLifecycleOnly ||
		streamed[0].AgentSessionID != "11111111-1111-4111-8111-111111111111" ||
		streamed[0].AgentSessionContract != "sshops-agent-v1" ||
		streamed[0].AgentSessionModel != "gpt-5.6-terra" ||
		!streamed[1].JobLifecycleOnly ||
		streamed[1].JobID != "job-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" ||
		streamed[1].JobPurpose != "download model weights" ||
		streamed[2].Command != "poll_background_job" || !streamed[3].JobLifecycleOnly ||
		streamed[3].JobID != "job-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" ||
		streamed[4].Disposition != "refused" {
		t.Fatalf("onStep did not stream steps live in order: %+v", streamed)
	}
	if steps[0].Command != "poll_background_job" || steps[0].Disposition != "ran" ||
		steps[0].ExitCode == nil || *steps[0].ExitCode != 0 || steps[0].Bytes != 42 {
		t.Fatalf("step[0] parsed wrong: %+v", steps[0])
	}
	if steps[0].JobID != "job-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || steps[0].JobState != "running" {
		t.Fatalf("background-job continuation metadata was dropped: %+v", steps[0])
	}
	if steps[0].JobPurpose != "download model weights" {
		t.Fatalf("background-job purpose was dropped: %+v", steps[0])
	}
	// refused command carried a null exit -> ExitCode must be nil, not 0 (0 would read as "ran clean")
	if steps[1].Disposition != "refused" || steps[1].ExitCode != nil {
		t.Fatalf("step[1] parsed wrong (exit should be nil): %+v (exit=%v)", steps[1], steps[1].ExitCode)
	}

	// no verdict markers -> empty Output, chatter ignored, no steps
	v2, s2, _, err := parseHarnessStream(strings.NewReader("just chatter\nno protocol here\n"), nil, nil)
	if err != nil {
		t.Fatalf("parse2: %v", err)
	}
	if v2 != "" || len(s2) != 0 {
		t.Fatalf("expected empty verdict/steps for protocol-less output, got %q / %d", v2, len(s2))
	}
}

func TestParseCurrentAgentSessionRequiresAppliedConversationAnchor(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	const workdirID = "22222222-2222-4222-8222-222222222222"
	without := `{"session_id":"` + id + `","workdir_id":"` + workdirID + `","contract":"` + opscontext.AgentSessionContract + `","model":"gpt-5.6-terra"}`
	if _, ok := parseAgentSessionUpdate(without); ok {
		t.Fatal("a current-contract receipt without an applied conversation anchor must not advance continuation")
	}
	with := `{"session_id":"` + id + `","workdir_id":"` + workdirID + `","contract":"` + opscontext.AgentSessionContract +
		`","model":"gpt-5.6-terra","conversation_anchor":"` + strings.Repeat("a", 64) + `"}`
	got, ok := parseAgentSessionUpdate(with)
	if !ok || got.AgentSessionConversationAnchor != strings.Repeat("a", 64) {
		t.Fatalf("valid current-contract receipt was rejected or lost its anchor: %+v ok=%v", got, ok)
	}
	if got.AgentSessionWorkdirID != workdirID {
		t.Fatalf("valid current-contract receipt lost its stable workdir id: %+v", got)
	}
	bad := strings.Replace(with, strings.Repeat("a", 64), strings.Repeat("g", 64), 1)
	if _, ok := parseAgentSessionUpdate(bad); ok {
		t.Fatal("non-hex applied anchor must be rejected")
	}
	uppercase := strings.Replace(with, strings.Repeat("a", 64), strings.Repeat("A", 64), 1)
	if _, ok := parseAgentSessionUpdate(uppercase); ok {
		t.Fatal("non-canonical uppercase anchor must be rejected")
	}
}

func TestParseHarnessStreamCaps(t *testing.T) {
	// step-count cap: excess @@STEP lines are dropped, not accumulated.
	var b strings.Builder
	for range maxHarnessSteps + 20 {
		b.WriteString(`@@STEP {"command":"x","tier":"read_only","disposition":"ran","exit":0,"bytes":1}` + "\n")
	}
	var streamedCap int
	_, steps, _, err := parseHarnessStream(strings.NewReader(b.String()), func(Step) { streamedCap++ }, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(steps) != maxHarnessSteps {
		t.Fatalf("step cap not enforced: got %d, want %d", len(steps), maxHarnessSteps)
	}
	// the live stream is bounded by the same cap — a firehose harness cannot flood onStep either
	if streamedCap != maxHarnessSteps {
		t.Fatalf("onStep not bounded by the step cap: fired %d, want %d", streamedCap, maxHarnessSteps)
	}

	// total-bytes cap: a verdict body past the ceiling (many bounded lines) fails closed.
	var big strings.Builder
	big.WriteString("<<<VERDICT>>>\n")
	filler := strings.Repeat("A", 1000) + "\n"
	for big.Len() < maxHarnessStdoutBytes+5000 {
		big.WriteString(filler)
	}
	big.WriteString("<<<END>>>\n")
	if _, _, _, err := parseHarnessStream(strings.NewReader(big.String()), nil, nil); err == nil {
		t.Fatalf("expected error when stdout exceeds %d bytes", maxHarnessStdoutBytes)
	}
}
