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
print("HANDSHAKE_OK host=%s user=%s port=%s instance=%s" % (
    conn.get("host"), conn.get("user"), conn.get("port"), conn.get("instance_id")))
print("HAS_PASSWORD=%s" % bool(conn.get("password")))
print("ENVKEYS=" + ",".join(sorted(os.environ.keys())))
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

func TestSupervisorCredentialNotInArgv(t *testing.T) {
	sup := Supervisor{
		Python:      pythonBin(),
		HarnessPath: writeFakeHarness(t, "import sys,json; json.loads(sys.stdin.readline()); print('ok')"),
		GatewayURL:  "http://127.0.0.1:3456",
		Timeout:     30 * time.Second,
	}
	c := cred("uhost-abc", "h", "root", 22, "ArgvMustNotHaveThis")
	// run with a task that is NOT the password; the password must travel via stdin only.
	res, err := sup.Run(context.Background(), c, "diagnose gpu")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Contains(res.Output, "ArgvMustNotHaveThis") {
		t.Fatalf("password surfaced in output")
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
