package sshops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func pythonBin() string {
	for _, c := range []string{"python3", "python"} {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return "python" // resolved via PATH by exec
}

const fakeEcho = `
import sys, json, os
line = sys.stdin.readline()
conn = json.loads(line)
print("<<<VERDICT>>>")
print("HANDSHAKE_OK host=%s user=%s port=%s instance=%s task=%s" % (
    conn.get("host"), conn.get("user"), conn.get("port"), conn.get("instance_id"), conn.get("task")))
print("HAS_PASSWORD=%s" % bool(conn.get("password")))
print("ENVKEYS=" + ",".join(sorted(os.environ.keys())))
print("<<<END>>>")
`

func TestSupervisorHandshakeAndScrubbedEnv(t *testing.T) {
	// a secret in the PARENT env that must NOT reach the child (proves env scrubbing)
	os.Setenv("LLM_API_KEY", "parent-secret-should-not-leak")
	os.Setenv("MYSQL_DSN", "user:pw@tcp/db")
	defer os.Unsetenv("LLM_API_KEY")
	defer os.Unsetenv("MYSQL_DSN")

	sup := Supervisor{
		Python:      pythonBin(),
		HarnessPath: writeFakeHarness(t, fakeEcho),
		GatewayURL:  "http://127.0.0.1:3456",
		Model:       "deepseek-v4-flash",
		Timeout:     30 * time.Second,
	}
	c := cred("uhost-abc", "1.2.3.4", "root", 23, "S3cr3tPw")

	res, err := sup.Run(context.Background(), c, "health check")
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
	// the child got the gateway config it needs
	if !strings.Contains(out, "ANTHROPIC_BASE_URL") {
		t.Fatalf("gateway env not passed: %q", out)
	}
}

func TestSupervisorCredentialAndTaskNotInArgv(t *testing.T) {
	// The fake echoes its own argv into the verdict block. Neither the credential nor the task may be
	// there: the credential never leaves stdin, and the task moved onto the stdin handshake too (it can
	// carry PII, and argv is visible to `ps` on the host).
	fake := "import sys,json; json.loads(sys.stdin.readline()); " +
		"print('<<<VERDICT>>>'); print('ARGV=' + ' '.join(sys.argv[1:])); print('<<<END>>>')"
	sup := Supervisor{
		Python:      pythonBin(),
		HarnessPath: writeFakeHarness(t, fake),
		GatewayURL:  "http://127.0.0.1:3456",
		Timeout:     30 * time.Second,
	}
	c := cred("uhost-abc", "h", "root", 22, "ArgvMustNotHaveThis")
	res, err := sup.Run(context.Background(), c, "diagnose gpu memory")
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
		Python:      pythonBin(),
		HarnessPath: writeFakeHarness(t, "import sys,time; sys.stdin.readline(); time.sleep(60); print('SHOULD_NOT_PRINT')"),
		GatewayURL:  "http://127.0.0.1:3456",
		Timeout:     1 * time.Second, // hard timeout kills the sleeping child
	}
	c := cred("x", "h", "root", 22, "pw")

	start := time.Now()
	res, err := sup.Run(context.Background(), c, "task")
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
	if _, err := (Supervisor{HarnessPath: "x"}).Run(context.Background(),
		Credential{Host: "h", User: "u", Port: 22}, "t"); err == nil {
		t.Fatalf("expected error for credential without secret")
	}
	if _, err := (Supervisor{}).Run(context.Background(),
		Credential{Host: "h", User: "u", Port: 22, password: "p"}, "t"); err == nil {
		t.Fatalf("expected error for missing harness path")
	}
}

func TestParseHarnessStream(t *testing.T) {
	in := strings.Join([]string{
		"claude cli starting...", // chatter -> ignored
		`@@STEP {"command":"nvidia-smi","tier":"read_only","disposition":"ran","exit":0,"bytes":42}`,
		`@@STEP {"command":"rm -rf /","tier":"destructive","disposition":"refused","exit":null,"bytes":0}`,
		"more chatter",
		"<<<VERDICT>>>",
		"GPU 健康。",
		"显存 512MiB 已用。",
		"<<<END>>>",
	}, "\n") + "\n"

	verdict, steps, err := parseHarnessStream(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if verdict != "GPU 健康。\n显存 512MiB 已用。" {
		t.Fatalf("verdict body wrong: %q", verdict)
	}
	if len(steps) != 2 {
		t.Fatalf("want 2 steps, got %d (%+v)", len(steps), steps)
	}
	if steps[0].Command != "nvidia-smi" || steps[0].Disposition != "ran" ||
		steps[0].ExitCode == nil || *steps[0].ExitCode != 0 || steps[0].Bytes != 42 {
		t.Fatalf("step[0] parsed wrong: %+v", steps[0])
	}
	// refused command carried a null exit -> ExitCode must be nil, not 0 (0 would read as "ran clean")
	if steps[1].Disposition != "refused" || steps[1].ExitCode != nil {
		t.Fatalf("step[1] parsed wrong (exit should be nil): %+v (exit=%v)", steps[1], steps[1].ExitCode)
	}

	// no verdict markers -> empty Output, chatter ignored, no steps
	v2, s2, err := parseHarnessStream(strings.NewReader("just chatter\nno protocol here\n"))
	if err != nil {
		t.Fatalf("parse2: %v", err)
	}
	if v2 != "" || len(s2) != 0 {
		t.Fatalf("expected empty verdict/steps for protocol-less output, got %q / %d", v2, len(s2))
	}
}

func TestParseHarnessStreamCaps(t *testing.T) {
	// step-count cap: excess @@STEP lines are dropped, not accumulated.
	var b strings.Builder
	for range maxHarnessSteps + 20 {
		b.WriteString(`@@STEP {"command":"x","tier":"read_only","disposition":"ran","exit":0,"bytes":1}` + "\n")
	}
	_, steps, err := parseHarnessStream(strings.NewReader(b.String()))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(steps) != maxHarnessSteps {
		t.Fatalf("step cap not enforced: got %d, want %d", len(steps), maxHarnessSteps)
	}

	// total-bytes cap: a verdict body past the ceiling (many bounded lines) fails closed.
	var big strings.Builder
	big.WriteString("<<<VERDICT>>>\n")
	filler := strings.Repeat("A", 1000) + "\n"
	for big.Len() < maxHarnessStdoutBytes+5000 {
		big.WriteString(filler)
	}
	big.WriteString("<<<END>>>\n")
	if _, _, err := parseHarnessStream(strings.NewReader(big.String())); err == nil {
		t.Fatalf("expected error when stdout exceeds %d bytes", maxHarnessStdoutBytes)
	}
}
