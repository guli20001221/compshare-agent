package sshops

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
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

// Result is what the supervisor returns to the engine. Output is the harness's scrubbed VERDICT body
// (protocol lines stripped); the credential is never present.
type Result struct {
	Output   string // the harness's scrubbed VERDICT body only — @@STEP lines and markers stripped
	Steps    []Step // one per command the harness ran/refused, in order (the activity stream + audit trail)
	ExitCode int
	TimedOut bool
}

// Step is one command the harness ran or refused, parsed from an @@STEP wire line. Metadata ONLY —
// never command output (INV-6) — so it is safe to surface as a live activity event or an audit row.
type Step struct {
	Command     string
	Tier        string
	Disposition string // "ran" | "refused" | "failed"
	ExitCode    *int   // nil for refused/failed commands that never produced an exit status
	Bytes       int
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

// Run spawns the harness for one consented task. cred must already be resolved via FetchCredential.
// The harness runs in its OWN process group, so a timeout/cancel kills the WHOLE tree — the python
// wrapper AND the claude CLI (+ node) the SDK spawns under it — not just the direct child. stdout is
// consumed through the bounded line protocol; only the VERDICT body becomes Output.
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
	// The task rides the stdin handshake, NOT argv: argv is visible to `ps` on the host and the task is
	// free-form operator/model text that may carry PII. The credential likewise travels only on stdin.
	cmd := exec.CommandContext(ctx, py, s.HarnessPath)
	cmd.Env = s.childEnv()
	setProcGroup(cmd)                                       // own process group (OS-specific)
	cmd.Cancel = func() error { return killProcGroup(cmd) } // on ctx-done kill the GROUP, not just python
	cmd.WaitDelay = 5 * time.Second                         // bound the post-kill wait if a pipe lingers

	handshake, err := json.Marshal(map[string]any{
		"host":        cred.Host,
		"user":        cred.User,
		"port":        cred.Port,
		"password":    cred.password, // plaintext -> stdin only, never logged/returned
		"instance_id": cred.InstanceID,
		"model":       s.Model,
		"task":        task, // NL request -> stdin, off the host process table
	})
	if err != nil {
		return Result{}, fmt.Errorf("sshops: marshal handshake: %w", err)
	}
	cmd.Stdin = bytes.NewReader(append(handshake, '\n'))

	// stdout carries the line protocol (@@STEP metadata + one VERDICT block); stderr carries the CLI's
	// own chatter / a Python traceback, surfaced only on failure. Neither can carry the credential — it
	// only ever travels on stdin. StdoutPipe + a bounded scanner streams stdout rather than buffering it
	// unboundedly, so a runaway harness cannot balloon this process's memory.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("sshops: stdout pipe: %w", err)
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("sshops: start harness: %w", err)
	}

	// Read stdout to EOF BEFORE Wait (Wait closes the pipe). On timeout/cancel the group is killed, the
	// pipe reaches EOF, and this returns.
	verdict, steps, parseErr := parseHarnessStream(stdout)
	runErr := cmd.Wait()
	_ = killProcGroup(cmd) // best-effort reap of any strays the SDK left in the group

	res := Result{Output: verdict, Steps: steps}
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
	if parseErr != nil {
		return res, fmt.Errorf("sshops: harness stream: %w", parseErr)
	}
	return res, nil
}

const (
	maxHarnessStdoutBytes = 1 << 20   // 1 MiB total stdout ceiling — verdict+steps are small; a firehose is a bug
	maxHarnessStepLine    = 256 << 10 // 256 KiB per-line cap — a step line is metadata; a huge one is malformed
	maxHarnessSteps       = 50        // step-count ceiling (mirrors the engine's maxInstanceOpsStepEvents)
)

// parseHarnessStream consumes the harness stdout line protocol and returns the terminal VERDICT body
// plus the ordered Steps. Two line shapes are trusted; everything else (the CLI's own chatter) is
// ignored. Bounded on three axes — total bytes, per-line size, step count — so a misbehaving harness
// cannot exhaust memory or flood the activity stream.
//
//	@@STEP {json}                one per command, metadata only
//	<<<VERDICT>>> ... <<<END>>>  the single terminal conclusion block
func parseHarnessStream(r io.Reader) (verdict string, steps []Step, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxHarnessStepLine)
	var total int
	var inVerdict bool
	var vb strings.Builder
	for sc.Scan() {
		line := sc.Text()
		total += len(line) + 1
		if total > maxHarnessStdoutBytes {
			return strings.TrimSpace(vb.String()), steps,
				fmt.Errorf("harness stdout exceeded %d bytes", maxHarnessStdoutBytes)
		}
		switch {
		case line == "<<<VERDICT>>>":
			inVerdict = true
		case line == "<<<END>>>":
			inVerdict = false
		case inVerdict:
			if vb.Len() > 0 {
				vb.WriteByte('\n')
			}
			vb.WriteString(line)
		case strings.HasPrefix(line, "@@STEP "):
			if len(steps) >= maxHarnessSteps {
				continue // cap the activity stream; the engine caps too (defense in depth)
			}
			if st, ok := parseStep(line[len("@@STEP "):]); ok {
				steps = append(steps, st)
			}
		}
	}
	if e := sc.Err(); e != nil {
		return strings.TrimSpace(vb.String()), steps, e
	}
	return strings.TrimSpace(vb.String()), steps, nil
}

func parseStep(payload string) (Step, bool) {
	var raw struct {
		Command     string `json:"command"`
		Tier        string `json:"tier"`
		Disposition string `json:"disposition"`
		Exit        *int   `json:"exit"`
		Bytes       int    `json:"bytes"`
	}
	if json.Unmarshal([]byte(payload), &raw) != nil {
		return Step{}, false
	}
	return Step{Command: raw.Command, Tier: raw.Tier, Disposition: raw.Disposition, ExitCode: raw.Exit, Bytes: raw.Bytes}, true
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
