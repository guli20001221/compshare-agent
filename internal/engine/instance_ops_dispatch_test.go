package engine

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/llm"
	"github.com/compshare-agent/internal/security"
	"github.com/compshare-agent/internal/tools"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

// fakeInstanceOpsRunner is a deterministic stand-in for the real subprocess+SSH
// runner. It records how often it was invoked (to prove the gates that must NOT
// run it actually don't), replays a fixed progress script, and returns a fixed
// verdict/error.
type fakeInstanceOpsRunner struct {
	progress []InstanceOpsProgress
	verdict  InstanceOpsVerdict
	err      error

	calls   int
	lastReq InstanceOpsRequest
}

func (f *fakeInstanceOpsRunner) Run(_ context.Context, req InstanceOpsRequest, onProgress func(InstanceOpsProgress)) (InstanceOpsVerdict, error) {
	f.calls++
	f.lastReq = req
	for _, p := range f.progress {
		onProgress(p)
	}
	return f.verdict, f.err
}

func alwaysConfirm(string, map[string]any) bool { return true }
func neverConfirm(string, map[string]any) bool  { return false }
func instanceOpsArgs() map[string]any {
	return map[string]any{"UHostId": "uhost-1", "Task": "排查掉卡"}
}
func captureSteps(dst *[]StepEvent) func(StepEvent) {
	return func(s StepEvent) { *dst = append(*dst, s) }
}

func newInstanceOpsEngine(runner InstanceOpsRunner, confirm ConfirmFunc) *Engine {
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{results: map[string]map[string]any{}}, confirm)
	eng.SetInstanceOps(runner)
	return eng
}

// 门 2 — the tool is in the model window ONLY when the lane is wired, and it is
// registered with the diagnosis execution route (so routeForAction agrees with
// the dispatch branch that catches it before the mutating handler).
func TestInstanceOps_ToolWindowGatedByLaneAndRoute(t *testing.T) {
	off := toolNames(centralAgentToolWindow(false, false))
	require.NotContains(t, off, "DiagnoseInstanceInternals",
		"lane off ⇒ tool must be absent from the window (INV-10)")

	on := toolNames(centralAgentToolWindow(false, true))
	require.Contains(t, on, "DiagnoseInstanceInternals",
		"lane on ⇒ tool must be visible to the model")

	capa, ok := tools.DefaultCapabilityRegistry().Lookup("DiagnoseInstanceInternals")
	require.True(t, ok, "tool must be registered in the capability registry")
	require.Equal(t, tools.ActionRouteDiagnosis, capa.Policy.Route,
		"tool must carry the diagnosis route so dispatch catches it before the mutating branch")
}

// 门 3 — a declined authorization card must NOT run the harness, must keep the
// turn going (no finalReplyPrefix), and must return the DECLINE text — distinct
// from the "already ran" text (V9).
func TestInstanceOps_DeclineDoesNotRunAndTurnContinues(t *testing.T) {
	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "unused"}}
	eng := newInstanceOpsEngine(runner, neverConfirm)

	var steps []StepEvent
	out := eng.executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", instanceOpsArgs(), captureSteps(&steps))

	require.Equal(t, 0, runner.calls, "declined card must not spawn the harness")
	require.False(t, strings.HasPrefix(out, finalReplyPrefix), "decline is non-terminal — the turn continues")
	require.Contains(t, out, "已取消")
	require.NotContains(t, out, "已经执行过", "decline text must differ from the already-ran text (V9)")
}

// 门 4 — with no confirm function installed the lane fails closed: no panic and
// the harness is never run (INV-7).
func TestInstanceOps_NilConfirmFailsClosed(t *testing.T) {
	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "unused"}}
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{results: map[string]map[string]any{}}, nil)
	eng.SetInstanceOps(runner)

	out := eng.executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", instanceOpsArgs(), noopStep)

	require.Equal(t, 0, runner.calls, "nil confirm ⇒ never fetch, never spawn")
	require.Contains(t, out, "无法进行授权确认")
}

// 门 5 — on success the verdict is the deterministic final reply: it survives the
// loop byte-for-byte (after the prefix strip), the model is NOT called again, and
// the turn identity is plumbed into the request (INV-9 dedup key).
func TestInstanceOps_VerdictSurvivesAsTerminalReply(t *testing.T) {
	sentinel := "根因：GPU 驱动与内核版本不匹配，建议重装驱动后重启实例。"
	require.Equal(t, sentinel, security.RedactOperationalTokensInText(sentinel),
		"sentinel must be redaction-invariant so this test proves rewrite-survival, not redaction")

	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: sentinel, Ran: 2}}
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("t1", "DiagnoseInstanceInternals", `{"UHostId":"uhost-1","Task":"排查掉卡"}`)}},
	}}
	eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{}}, alwaysConfirm)
	eng.SetInstanceOps(runner)

	reply, err := eng.Chat(context.Background(), "我的实例掉卡了", noopStep)
	require.NoError(t, err)
	require.Equal(t, sentinel, reply, "the harness verdict must be the final reply, unrewritten")
	require.Equal(t, 1, runner.calls)
	require.Len(t, model.calls, 1, "finalReplyPrefix terminates the turn — no synthesis round after the verdict")
	require.NotEmpty(t, runner.lastReq.TurnID, "the turn identity must reach the runner as the audit dedup key")
}

// 门 6 — the activity stream shape: connected + each command + one summary; refused
// commands ride StepBlocked; no message carries an N/M denominator (no fake progress bar).
func TestInstanceOps_ActivityStreamShape(t *testing.T) {
	exit0 := 0
	runner := &fakeInstanceOpsRunner{
		progress: []InstanceOpsProgress{
			{Kind: InstanceOpsProgressConnected},
			{Kind: InstanceOpsProgressCommand, Command: "nvidia-smi", Disposition: "ran", ExitCode: &exit0, Bytes: 412},
			{Kind: InstanceOpsProgressCommand, Command: "nvidia-smi -L", Disposition: "ran", ExitCode: &exit0, Bytes: 88},
			{Kind: InstanceOpsProgressCommand, Command: "cat /proc/driver/nvidia/version", Disposition: "ran", ExitCode: &exit0, Bytes: 120},
			{Kind: InstanceOpsProgressCommand, Command: "modprobe nvidia", Disposition: "refused"},
			{Kind: InstanceOpsProgressCommand, Command: "rm -rf /", Disposition: "refused"},
		},
		verdict: InstanceOpsVerdict{Text: "done", Ran: 3, Refused: 2},
	}
	eng := newInstanceOpsEngine(runner, alwaysConfirm)

	var steps []StepEvent
	eng.executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", instanceOpsArgs(), captureSteps(&steps))

	require.Len(t, steps, 7, "1 connected + 5 commands + 1 summary")

	blocked := 0
	denom := regexp.MustCompile(`\d+/\d+`)
	for _, s := range steps {
		if s.Type == StepBlocked {
			blocked++
		}
		require.False(t, denom.MatchString(s.Message), "no N/M denominator (no fake progress bar): %q", s.Message)
		require.Nil(t, s.Args, "activity steps must not carry args")
	}
	require.Equal(t, 2, blocked, "both refused commands must ride StepBlocked, not StepError (trace-sink constant)")
}

// 门 7 — the engine caps per-command events at maxInstanceOpsStepEvents. Under the
// cap every command shows; over the cap the total is bounded (cap + connected + summary).
func TestInstanceOps_StepEventsBoundedByCap(t *testing.T) {
	build := func(n int) *fakeInstanceOpsRunner {
		progress := []InstanceOpsProgress{{Kind: InstanceOpsProgressConnected}}
		for range n {
			progress = append(progress, InstanceOpsProgress{Kind: InstanceOpsProgressCommand, Command: "ls", Disposition: "ran"})
		}
		return &fakeInstanceOpsRunner{progress: progress, verdict: InstanceOpsVerdict{Text: "v", Ran: n}}
	}

	// 45 (near the real harness max_turns=40 boundary) → all shown.
	var under []StepEvent
	newInstanceOpsEngine(build(45), alwaysConfirm).
		executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", instanceOpsArgs(), captureSteps(&under))
	require.Len(t, under, 47, "1 connected + 45 commands + 1 summary")

	// 60 → per-command events capped at 50; total ≤ 50 + connected + summary.
	var over []StepEvent
	newInstanceOpsEngine(build(60), alwaysConfirm).
		executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", instanceOpsArgs(), captureSteps(&over))
	require.LessOrEqual(t, len(over), maxInstanceOpsStepEvents+2)
}

// 门 8b — a no-SSH-target instance (e.g. Windows: empty SshLoginCommand) is refused
// HONESTLY and NON-retryably. The card was authorized, so the runner IS reached and
// returns ErrInstanceOpsNoSSHTarget; the engine must NOT surface the generic
// "请稍后重试" text for a box that can never be entered.
func TestInstanceOps_NoSSHTargetRefusedHonestly(t *testing.T) {
	runner := &fakeInstanceOpsRunner{err: ErrInstanceOpsNoSSHTarget}
	eng := newInstanceOpsEngine(runner, alwaysConfirm)

	var steps []StepEvent
	out := eng.executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", instanceOpsArgs(), captureSteps(&steps))

	require.Equal(t, 1, runner.calls, "the card was authorized, so the runner is reached")
	require.True(t, strings.HasPrefix(out, finalReplyPrefix), "an unenterable box is a terminal refusal")
	require.Contains(t, out, "没有 SSH 登录入口", "the refusal must name the real cause")
	require.NotContains(t, out, "请稍后重试", "must not imply a transient, retryable failure")
}

// 门 8 — at most one in-instance run per turn, even if the model tweaks one word of
// the Task to dodge the DB dedup key. The second call returns the "already ran"
// text, distinct from the decline text (V9).
func TestInstanceOps_OnePerTurnEvenWithTaskTweak(t *testing.T) {
	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "结论", Ran: 1}}
	eng := newInstanceOpsEngine(runner, alwaysConfirm)
	ctx := context.Background()

	out1 := eng.executeInstanceOps(ctx, "DiagnoseInstanceInternals", map[string]any{"UHostId": "uhost-1", "Task": "排查掉卡"}, noopStep)
	out2 := eng.executeInstanceOps(ctx, "DiagnoseInstanceInternals", map[string]any{"UHostId": "uhost-1", "Task": "排查掉卡问题"}, noopStep)

	require.Equal(t, 1, runner.calls, "one-word Task tweak must not buy a second in-instance run")
	require.True(t, strings.HasPrefix(out1, finalReplyPrefix), "first run returns the terminal verdict")
	require.Contains(t, out2, "已经执行过")
	require.False(t, strings.HasPrefix(out2, finalReplyPrefix), "the already-ran refusal keeps the turn going")
}

// V9/INV-11 coupling — a DECLINE still spends the turn's single slot (repeatedly
// re-prompting the user is worse than answering once), so a second call after a
// decline gets the already-ran text, not a fresh card.
func TestInstanceOps_DeclineStillConsumesTurnSlot(t *testing.T) {
	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "unused"}}
	eng := newInstanceOpsEngine(runner, neverConfirm)
	ctx := context.Background()

	out1 := eng.executeInstanceOps(ctx, "DiagnoseInstanceInternals", instanceOpsArgs(), noopStep)
	out2 := eng.executeInstanceOps(ctx, "DiagnoseInstanceInternals", instanceOpsArgs(), noopStep)

	require.Equal(t, 0, runner.calls)
	require.Contains(t, out1, "已取消")
	require.Contains(t, out2, "已经执行过", "the declined slot is spent — the second call is not a fresh card")
}
