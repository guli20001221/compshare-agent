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

	"github.com/compshare-agent/internal/opscontext"
)

// Supervisor spawns the Python Agent-SDK harness once per consented ops-task and feeds it the SSH
// credential over a one-shot stdin handshake. Security properties:
//   - the credential crosses ONLY via stdin (a JSON line) — never argv, never an env var. The SDK
//     passes the wrapper's whole environment into the `claude` CLI it spawns, so the child gets an
//     explicitly constructed minimal environment rather than inheriting the server's environment.
//   - one resolved ModelVerse token (dedicated, or the configured answer-key fallback) is passed as
//     ANTHROPIC_AUTH_TOKEN; every unrelated server secret and the SSH credential are excluded.
//   - the process lifetime is the task: ctx cancellation / timeout kills it (CommandContext), so
//     the heap-resident credential dies with it. No reuse, no caching.
//   - only the harness's already-scrubbed stdout is returned; the credential is never in it.
type Supervisor struct {
	Python      string        // interpreter; default "python3"
	HarnessPath string        // absolute path to harness.py
	BaseURL     string        // ANTHROPIC_BASE_URL (for production: https://api.modelverse.cn)
	APIKey      string        // ModelVerse token passed only to the Claude CLI child
	Model       string        // ModelVerse model id (e.g. gpt-5.6-terra)
	Timeout     time.Duration // hard wall-clock per task; default 5m
}

// Keep the core service contract compiler-enforced. Run remains a public compatibility wrapper,
// but all Service paths use RunWithContext so a new runner cannot quietly drop model context.
var _ harnessRunner = Supervisor{}

// ConfirmRequest is one pending write the harness will not run until a human answers. Command is
// the LITERAL string it is about to execute — the card must show exactly what runs, or the approval
// describes something else.
type ConfirmRequest struct {
	ID      string `json:"id"`
	Command string `json:"command"`
}

// ConfirmDecision is the terminal result of a per-command confirmation card.
//
// A bool alone made three materially different outcomes indistinguishable: the user declined, the
// card timed out, or the client disconnected. All must deny the write, but collapsing them caused a
// timeout to be rendered as "you declined" in the activity stream. TerminalReason is the existing
// transport-level closed-set spelling (user_declined | timeout | client_disconnect |
// delivery_failed | broker_cancelled); an empty or unknown value deliberately degrades to the legacy
// "no approval received" reason in the harness.
type ConfirmDecision struct {
	Approved       bool
	TerminalReason string
}

// ConfirmFunc asks the user about one write. It must block until answered, and returns the terminal
// decision so the harness can preserve WHY an unapproved command did not execute.
type ConfirmFunc func(ConfirmRequest) ConfirmDecision

type confirmReply struct {
	ID             string `json:"id"`
	Approved       bool   `json:"approved"`
	TerminalReason string `json:"terminal_reason,omitempty"`
}

// Result is what the supervisor returns to the engine. Output is the harness's scrubbed VERDICT body
// (protocol lines stripped); the credential is never present.
type Result struct {
	Output   string // the harness's scrubbed VERDICT body only — @@STEP lines and markers stripped
	Steps    []Step // one per command the harness ran/refused, in order (the activity stream + audit trail)
	ExitCode int
	TimedOut bool
	// PreflightFailed reports that the dial never landed, so NO command ran and the verdict is a
	// refusal notice rather than a diagnosis. It comes from the harness's @@OUTCOME line, not from
	// guessing at len(Steps): a run can legitimately execute zero commands, and inferring the
	// disposition from that would relabel a real diagnosis as a failed dial.
	//
	// The zero value means "entered", so a harness that predates @@OUTCOME behaves exactly as before.
	PreflightFailed bool
	// ErrClass is the credential-free failure class behind PreflightFailed — paramiko's exception type
	// name, calibrated in _DIAL_CLASS_REASONS: TimeoutError = packets dropped, NoValidConnectionsError
	// = actively refused, gaierror = DNS. Empty on a run that entered the box.
	ErrClass string
	// ContextApplied is true only when the harness confirmed — after the model turn had actually begun
	// on that prompt, not merely after building it — that it included the independently transported
	// context. It is deliberately false for old harnesses, bounded fallbacks, and any SDK failure
	// before the first model message, so the finished audit row never overstates delivery.
	ContextApplied bool
}

// Step is one command the harness ran or refused, parsed from an @@STEP wire line. Metadata ONLY —
// never command output (INV-6) — so it is safe to surface as a live activity event or an audit row.
type Step struct {
	Command     string
	Tier        string
	Disposition string // "ran" | "refused" | "failed"
	// Reason is the harness's own fine-grained disposition ("refused_destructive",
	// "refused_form", "refused_confirmation_timeout", …) — the fact Disposition throws away. The set
	// is open by design: consumers map what they know and degrade on the rest. Empty when
	// the harness predates the field, which is why every consumer must degrade rather than switch
	// exhaustively on it.
	Reason   string
	ExitCode *int // nil for refused/failed commands that never produced an exit status
	Bytes    int
}

// envAllowlist: non-secret system vars the interpreter / claude CLI need to function. Deliberately
// excludes server secrets (AK/SK, MYSQL_DSN, LLM_API_KEY, COMPSHARE_*). The one ModelVerse token the
// CLI needs is inserted explicitly by childEnv; the SSH credential is NOT here and goes via stdin.
var envAllowlist = []string{
	"PATH", "Path", "HOME", "USERPROFILE", "SYSTEMROOT", "SystemRoot",
	"TEMP", "TMP", "TMPDIR", "LANG", "LC_ALL", "PYTHONPATH", "PYTHONHOME",
	"WINDIR", "COMSPEC", "PATHEXT",
}

func (s Supervisor) childEnv() []string {
	env := []string{
		"ANTHROPIC_BASE_URL=" + s.BaseURL,
		"ANTHROPIC_AUTH_TOKEN=" + s.APIKey,
		// ModelVerse recommends disabling experimental Claude Code beta headers for compatibility.
		"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1",
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
// consumed through the bounded line protocol; only the VERDICT body becomes Output. onStep, if
// non-nil, fires once per command as its @@STEP line is parsed — the LIVE activity stream, metadata
// only (INV-6). The same Steps are also returned in Result.Steps for the caller's tally/audit.
func (s Supervisor) Run(ctx context.Context, cred Credential, task string, onStep func(Step), onConfirm ConfirmFunc) (Result, error) {
	return s.RunWithContext(ctx, cred, task, opscontext.Context{}, onStep, onConfirm)
}

// RunWithContext is Run with a separately serialized reference context. It is
// not placed in argv or merged into task, so host process listings and task-hash
// semantics retain their previous boundaries.
func (s Supervisor) RunWithContext(ctx context.Context, cred Credential, task string, modelContext opscontext.Context, onStep func(Step), onConfirm ConfirmFunc) (Result, error) {
	if s.HarnessPath == "" {
		return Result{}, fmt.Errorf("sshops: no harness path configured")
	}
	if s.BaseURL == "" {
		return Result{}, fmt.Errorf("sshops: no Anthropic base URL configured")
	}
	if s.APIKey == "" {
		return Result{}, fmt.Errorf("sshops: no Anthropic API key configured")
	}
	if !cred.HasSecret() {
		return Result{}, fmt.Errorf("sshops: credential has no secret")
	}
	timeout := s.Timeout
	if timeout <= 0 {
		// Wall clock must cover the WHOLE command sequence, not a typical one. ssh_transport caps a
		// A diagnosis may run 20-45 commands, including expensive filesystem reads. Size the wall
		// clock for that sequence; each step remains visible while it runs.
		timeout = 12 * time.Minute
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
		"context":     modelContext,
		// Private destinations for the structured endpoint probe. Context itself omits these fields,
		// so URLs carrying console tokens and raw hosts never enter the model prompt or audit record.
		// The harness exposes only opaque IDs and resolves them against this stdin-only list.
		"endpoint_targets": modelContext.EndpointTargets,
	})
	if err != nil {
		return Result{}, fmt.Errorf("sshops: marshal handshake: %w", err)
	}
	// stdin stays OPEN for the whole run. It carried only the handshake before; now it is also the
	// return path for per-command approvals, so it cannot be a one-shot reader. The credential is
	// still the first and only secret on it, and nothing is ever read back from this direction — the
	// harness talks on stdout, we answer on stdin, and the two never swap roles.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Result{}, fmt.Errorf("sshops: stdin pipe: %w", err)
	}

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
	if _, err := stdin.Write(append(handshake, '\n')); err != nil {
		_ = killProcGroup(cmd)
		return Result{}, fmt.Errorf("sshops: write handshake: %w", err)
	}
	// Closing stdin is what tells a harness blocked on an approval that no answer is coming; its
	// readline returns EOF and it denies. So the close must happen on EVERY exit path from the
	// stream loop, not only the happy one.
	defer func() { _ = stdin.Close() }()

	// Read stdout to EOF BEFORE Wait (Wait closes the pipe). On timeout/cancel the group is killed, the
	// pipe reaches EOF, and this returns. onStep fires live as each @@STEP line arrives (the harness
	// flushes per command), so the caller sees the activity stream during the run, not after it.
	// answer serialises one approval round-trip: the parser is single-threaded, so a request is
	// always fully answered before the next line is read.
	answer := func(req ConfirmRequest) {
		decision := ConfirmDecision{}
		if onConfirm != nil { // no confirmer wired is not "allow" — it is a lane with no human on it
			decision = onConfirm(req)
		}
		reply, mErr := json.Marshal(confirmReply{
			ID:             req.ID,
			Approved:       decision.Approved,
			TerminalReason: strings.TrimSpace(decision.TerminalReason),
		})
		if mErr != nil {
			return // the harness blocks, then EOFs on the deferred close and denies
		}
		_, _ = stdin.Write(append(reply, '\n'))
	}
	verdict, steps, outcome, parseErr := parseHarnessStream(stdout, onStep, answer)
	runErr := cmd.Wait()
	_ = killProcGroup(cmd) // best-effort reap of any strays the SDK left in the group

	res := Result{
		Output:          verdict,
		Steps:           steps,
		PreflightFailed: outcome.Outcome == outcomePreflightFailed,
		ErrClass:        outcome.ErrClass,
		ContextApplied:  outcome.ContextApplied,
	}
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
	// Step-count ceiling. MUST stay >= the harness's turn budget (DEFAULT_MAX_TURNS in harness.py):
	// a cap below it silently truncates the tail of the activity stream, so the audit tally under-counts
	// and the user stops seeing commands while the agent is still running.
	maxHarnessSteps = 120
)

// parseHarnessStream consumes the harness stdout line protocol and returns the terminal VERDICT body
// plus the ordered Steps. Two line shapes are trusted; everything else (the CLI's own chatter) is
// ignored. Bounded on three axes — total bytes, per-line size, step count — so a misbehaving harness
// cannot exhaust memory or flood the activity stream.
//
//	@@STEP {json}                one per command, metadata only
//	<<<VERDICT>>> ... <<<END>>>  the single terminal conclusion block
//
// onStep, if non-nil, is invoked once per parsed @@STEP as it is read (bounded by the same step cap),
// so a caller can surface a live activity stream instead of waiting for the whole run to finish.
// outcomePreflightFailed is the only @@OUTCOME value the harness emits today. Unknown values are kept
// verbatim in harnessOutcome.Outcome but map to "entered", so a newer harness inventing a value cannot
// silently turn a successful diagnosis into a refusal on an older supervisor.
const outcomePreflightFailed = "preflight_failed"

// harnessOutcome is the parsed @@OUTCOME line. An absent line still means the box was entered, but
// it cannot prove an independently transported context reached the model, so ContextApplied is false.
type harnessOutcome struct {
	Outcome        string `json:"outcome"`
	ErrClass       string `json:"err_class"`
	ContextApplied bool   `json:"context_applied"`
}

func parseHarnessStream(r io.Reader, onStep func(Step), onConfirm func(ConfirmRequest)) (verdict string, steps []Step, outcome harnessOutcome, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxHarnessStepLine)
	var total int
	var inVerdict bool
	var vb strings.Builder
	for sc.Scan() {
		line := sc.Text()
		total += len(line) + 1
		if total > maxHarnessStdoutBytes {
			return strings.TrimSpace(vb.String()), steps, outcome,
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
		case strings.HasPrefix(line, "@@CONFIRM "):
			// The harness is blocked until this is answered, so an unparseable request must still
			// produce a reply — with an empty id, which the harness rejects and reads as a denial.
			// Dropping it instead would hang the run until the wall-clock timeout.
			var req ConfirmRequest
			if json.Unmarshal([]byte(line[len("@@CONFIRM "):]), &req) != nil {
				req = ConfirmRequest{}
			}
			if onConfirm != nil {
				onConfirm(req)
			}
		case strings.HasPrefix(line, "@@STEP "):
			if len(steps) >= maxHarnessSteps {
				continue // cap the activity stream; the engine caps too (defense in depth)
			}
			if st, ok := parseStep(line[len("@@STEP "):]); ok {
				steps = append(steps, st)
				if onStep != nil {
					onStep(st) // live: fire as parsed, bounded by the same step cap above
				}
			}
		case strings.HasPrefix(line, "@@OUTCOME "):
			// Last one wins, and an unparseable payload leaves the zero value — i.e. "entered".
			// Failing open here is deliberate: the disposition it feeds is an audit label, and
			// mislabelling a real diagnosis as a dial that never happened is the worse error.
			var oc harnessOutcome
			if json.Unmarshal([]byte(line[len("@@OUTCOME "):]), &oc) == nil {
				outcome = oc
			}
		}
	}
	if e := sc.Err(); e != nil {
		return strings.TrimSpace(vb.String()), steps, outcome, e
	}
	return strings.TrimSpace(vb.String()), steps, outcome, nil
}

func parseStep(payload string) (Step, bool) {
	var raw struct {
		Command     string `json:"command"`
		Tier        string `json:"tier"`
		Disposition string `json:"disposition"`
		Reason      string `json:"reason"`
		Exit        *int   `json:"exit"`
		Bytes       int    `json:"bytes"`
	}
	if json.Unmarshal([]byte(payload), &raw) != nil {
		return Step{}, false
	}
	return Step{Command: raw.Command, Tier: raw.Tier, Disposition: raw.Disposition,
		Reason: raw.Reason, ExitCode: raw.Exit, Bytes: raw.Bytes}, true
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
