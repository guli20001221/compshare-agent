package sshops

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// Supervisor spawns the Python Agent-SDK harness once per consented ops-task and feeds it the SSH
// credential over a one-shot stdin handshake. Security properties:
//   - the credential crosses ONLY via stdin (a JSON line) — never argv, never an env var. The SDK
//     passes the wrapper's whole environment into the `claude` CLI it spawns, so the child runs
//     with a MINIMAL allowlisted env (no server AK/SK/MYSQL_DSN/LLM key, no credential).
//   - the process lifetime is the task: ctx cancellation / timeout kills it (CommandContext), so
//     the heap-resident credential dies with it. No reuse, no caching.
//   - only the harness's already-scrubbed stdout is returned; the credential is never in it.
type Supervisor struct {
	Python      string        // interpreter; default "python3"
	HarnessPath string        // absolute path to harness.py
	GatewayURL  string        // ANTHROPIC_BASE_URL of the local claude-code-router gateway
	Model       string        // third-party model id (e.g. deepseek-v4-flash)
	Timeout     time.Duration // hard wall-clock per task; default 5m
}

// Result is what the supervisor returns to the engine. Output is the harness's scrubbed diagnosis
// text; the credential is never present.
type Result struct {
	Output   string
	ExitCode int
	TimedOut bool
}

// envAllowlist: non-secret system vars the interpreter / claude CLI need to function. Deliberately
// excludes everything secret-bearing (AK/SK, MYSQL_DSN, LLM_API_KEY, COMPSHARE_*). The credential
// is NOT here — it goes via stdin only.
var envAllowlist = []string{
	"PATH", "Path", "HOME", "USERPROFILE", "SYSTEMROOT", "SystemRoot",
	"TEMP", "TMP", "TMPDIR", "LANG", "LC_ALL", "PYTHONPATH", "PYTHONHOME",
	"WINDIR", "COMSPEC", "PATHEXT",
}

func (s Supervisor) childEnv() []string {
	env := []string{
		"ANTHROPIC_BASE_URL=" + s.GatewayURL,
		"ANTHROPIC_API_KEY=dummy-unused",
		"NO_PROXY=127.0.0.1,localhost",
		"no_proxy=127.0.0.1,localhost",
		// Force UTF-8 on the harness's stdio so its Chinese verdict (and any emoji) survive a
		// CJK-locale host (e.g. GBK Windows), and so the captured bytes decode cleanly back here.
		"PYTHONIOENCODING=utf-8",
	}
	for _, k := range envAllowlist {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// APIEndpoint is the per-task loopback api_read proxy the harness pulls read-only API data from in
// api mode (destination-B). It carries NO credential — the Go proxy holds the signed executor.
type APIEndpoint struct {
	URL     string
	Token   string
	Actions []string
}

// Run spawns the harness for one consented SSH task. cred must already be resolved via FetchCredential.
func (s Supervisor) Run(ctx context.Context, cred Credential, task string) (Result, error) {
	if s.HarnessPath == "" {
		return Result{}, fmt.Errorf("sshops: no harness path configured")
	}
	if !cred.HasSecret() {
		return Result{}, fmt.Errorf("sshops: credential has no secret")
	}
	return s.spawn(ctx, task, map[string]any{
		"host":        cred.Host,
		"user":        cred.User,
		"port":        cred.Port,
		"password":    cred.password, // plaintext -> stdin only, never logged/returned
		"instance_id": cred.InstanceID,
		"model":       s.Model,
	})
}

// RunAPI spawns the harness in API-only mode (destination-B): no SSH credential; the agent's sole
// tool is api_read pointed at the per-task loopback proxy. Used by API-only diagnoses (e.g. billing).
func (s Supervisor) RunAPI(ctx context.Context, api APIEndpoint, task string) (Result, error) {
	if s.HarnessPath == "" {
		return Result{}, fmt.Errorf("sshops: no harness path configured")
	}
	if api.URL == "" || api.Token == "" {
		return Result{}, fmt.Errorf("sshops: api endpoint incomplete")
	}
	return s.spawn(ctx, task, map[string]any{
		"mode":        "api",
		"api_url":     api.URL,
		"api_token":   api.Token,
		"api_actions": api.Actions,
		"model":       s.Model,
	})
}

// spawn runs the harness once with the given stdin handshake and captures its scrubbed stdout.
// Shared by Run (ssh) and RunAPI (api). The handshake is the ONLY inbound channel — never argv,
// never an env var; the SSH password (ssh mode) lives only here and in the transport.
func (s Supervisor) spawn(ctx context.Context, task string, handshake map[string]any) (Result, error) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	py := s.Python
	if py == "" {
		py = "python3"
	}
	// task is the NL diagnosis request (not a secret) — fine as argv. The CREDENTIAL is not argv.
	cmd := exec.CommandContext(ctx, py, s.HarnessPath, task)
	cmd.Env = s.childEnv()

	line, err := json.Marshal(handshake)
	if err != nil {
		return Result{}, fmt.Errorf("sshops: marshal handshake: %w", err)
	}
	cmd.Stdin = bytes.NewReader(append(line, '\n'))

	// Keep stdout (the agent's scrubbed verdict + AUDIT) separate from stderr (the claude CLI's
	// own startup chatter / a Python traceback). Only stdout is the diagnosis Output; stderr is
	// surfaced only on failure. Neither can carry the credential — it only ever travels on stdin.
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	res := Result{Output: out.String()}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		return res, fmt.Errorf("sshops: harness timed out after %s", timeout)
	}
	if runErr != nil {
		return res, fmt.Errorf("sshops: harness exited: %w; stderr: %s", runErr, tailString(errBuf.String(), 2000))
	}
	return res, nil
}

// tailString returns the last n bytes of s (rune-safe-ish: trims to a valid UTF-8 boundary).
func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	t := s[len(s)-n:]
	for len(t) > 0 && t[0]&0xC0 == 0x80 { // skip into a UTF-8 continuation byte
		t = t[1:]
	}
	return "…" + t
}
