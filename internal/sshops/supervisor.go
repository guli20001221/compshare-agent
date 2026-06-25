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
	}
	for _, k := range envAllowlist {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// Run spawns the harness for one consented task. cred must already be resolved via FetchCredential.
func (s Supervisor) Run(ctx context.Context, cred Credential, task string) (Result, error) {
	if s.HarnessPath == "" {
		return Result{}, fmt.Errorf("sshops: no harness path configured")
	}
	if !cred.HasSecret() {
		return Result{}, fmt.Errorf("sshops: credential has no secret")
	}
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

	handshake, err := json.Marshal(map[string]any{
		"host":        cred.Host,
		"user":        cred.User,
		"port":        cred.Port,
		"password":    cred.password, // plaintext -> stdin only, never logged/returned
		"instance_id": cred.InstanceID,
		"model":       s.Model,
	})
	if err != nil {
		return Result{}, fmt.Errorf("sshops: marshal handshake: %w", err)
	}
	cmd.Stdin = bytes.NewReader(append(handshake, '\n'))

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

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
		return res, fmt.Errorf("sshops: harness exited: %w", runErr)
	}
	return res, nil
}
