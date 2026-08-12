package engine

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/compshare-agent/internal/capability"
	"github.com/compshare-agent/internal/intent"
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
	// Direct dispatch tests still need the same target-proof precondition as a
	// real Chat turn. This explicit id is user-authored, not a fabricated list
	// referent; tests for the ambiguous-list failure live below.
	eng.lastUserMsg = "请排查 uhost-1"
	eng.turnContextViewThisTurn = AgentContext{CurrentQuestion: eng.lastUserMsg}
	eng.turnContextViewReady = true
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
	eng.lastUserMsg = "请排查 uhost-1"
	eng.turnContextViewThisTurn = AgentContext{CurrentQuestion: eng.lastUserMsg}
	eng.turnContextViewReady = true

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

	reply, err := eng.Chat(context.Background(), "我的 uhost-1 实例掉卡了", noopStep)
	require.NoError(t, err)
	require.Equal(t, sentinel, reply, "the harness verdict must be the final reply, unrewritten")
	require.Equal(t, 1, runner.calls)
	require.Len(t, model.calls, 1, "finalReplyPrefix terminates the turn — no synthesis round after the verdict")
	require.NotEmpty(t, runner.lastReq.TurnID, "the turn identity must reach the runner as the audit dedup key")
}

// Regression: a cold session compiles its turn context before the model's first
// read. If that read proves the account has exactly one instance, a vague
// diagnostic request has no list-selection ambiguity and may enter the lane.
// This must work in the same Chat turn; requiring a second user message merely
// because the registry became fresh a few milliseconds late is a real UX loss.
func TestInstanceOps_AllowsSameTurnFreshSingleRegistry(t *testing.T) {
	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "已确认 ComfyUI 服务未运行。"}}
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("list", capability.ReadToolName(intent.IntentResourceInfo), `{}`)}},
		{ToolCalls: []openai.ToolCall{toolCall("diagnose", "DiagnoseInstanceInternals", `{"UHostId":"uhost-only","Task":"排查 ComfyUI 无法打开"}`)}},
	}}
	executor := &mockExecutor{results: map[string]map[string]any{
		"DescribeCompShareInstance": {"TotalCount": float64(1), "UHostSet": []any{map[string]any{
			"UHostId": "uhost-only", "Name": "training-4090", "State": "Running", "GpuType": "4090", "GPU": float64(1), "CPU": float64(8), "Memory": float64(64),
		}}},
	}}
	confirmCalls := 0
	eng := NewWithDeps(model, executor, func(string, map[string]any) bool {
		confirmCalls++
		return true
	})
	eng.SetInstanceOps(runner)

	reply, err := eng.Chat(context.Background(), "ComfyUI 打不开了", noopStep)

	require.NoError(t, err)
	id, _ := eng.singleRegistryInstance()
	require.Equal(t, "uhost-only", id, "premise: the first read must have refreshed a complete singleton registry")
	require.Equal(t, "已确认 ComfyUI 服务未运行。", reply)
	require.Equal(t, 1, runner.calls, "a same-turn complete singleton registry is a proof, not a list-row guess")
	require.Equal(t, 1, confirmCalls)
	require.Len(t, model.calls, 2)
}

func TestInstanceOps_SingleRegistryNeverOverridesAnUnresolvedExplicitTarget(t *testing.T) {
	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "should not run"}}
	confirmCalls := 0
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{results: map[string]map[string]any{}}, func(string, map[string]any) bool {
		confirmCalls++
		return true
	})
	eng.SetInstanceOps(runner)
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(1), "UHostSet": []any{map[string]any{
			"UHostId": "uhost-only", "Name": "comfyui", "State": "Running",
		}},
	}, "test"))
	eng.turnContextViewThisTurn = AgentContext{CurrentQuestion: "请排查 uhost-does-not-exist"}
	eng.turnContextViewReady = true

	out := eng.executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", map[string]any{
		"UHostId": "uhost-only", "Task": "排查 ComfyUI 无法打开",
	}, noopStep)

	require.Zero(t, runner.calls, "an explicit unresolved id must never be replaced with the account singleton")
	require.Zero(t, confirmCalls)
	require.Contains(t, out, "请先明确要排查的实例")
}

// Regression for the production incident: after an account-wide list, a model
// picked the first running row for a vague "镜像无法运行" symptom. A list is an
// observation, not a selection, so no card and no harness entry may follow.
func TestInstanceOps_RejectsSelfElectedTargetFromAccountList(t *testing.T) {
	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "should not run"}}
	confirmCalls := 0
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{results: map[string]map[string]any{}}, func(string, map[string]any) bool {
		confirmCalls++
		return true
	})
	eng.SetInstanceOps(runner)
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(3),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-first", "Name": "image", "State": "Running"},
			map[string]any{"UHostId": "uhost-second", "Name": "train", "State": "Running"},
			map[string]any{"UHostId": "uhost-third", "Name": "old", "State": "Stopped"},
		},
	}, "test"))
	eng.lastUserMsg = "实例开启后，镜像没办法正常运行"
	eng.turnContextViewThisTurn = (ContextCompiler{}).CompileForTurn(eng, eng.lastUserMsg, "turn-vague-image", time.Now())
	eng.turnContextViewReady = true

	var steps []StepEvent
	out := eng.executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", map[string]any{
		"UHostId": "uhost-first", "Task": "排查镜像无法运行",
	}, captureSteps(&steps))

	require.Zero(t, runner.calls, "an account-list row is never permission to enter that instance")
	require.Zero(t, confirmCalls, "an ambiguous target must not show an authorization card")
	require.False(t, eng.instanceOpsRanThisTurn, "a rejected target did not enter the lane and must not spend the run slot")
	require.Contains(t, out, "请先明确要排查的实例")
	require.NotEmpty(t, steps)
	require.Equal(t, StepBlocked, steps[0].Type)
}

func TestInstanceOps_VagueToolCallReturnsToTheAgentForTargetSelection(t *testing.T) {
	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "should not run"}}
	confirmCalls := 0
	model := &mockLLM{responses: []llm.ChatResponse{
		{ToolCalls: []openai.ToolCall{toolCall("t-vague", "DiagnoseInstanceInternals", `{"UHostId":"uhost-first","Task":"排查镜像无法运行"}`)}},
		{Content: "请告诉我要排查哪台实例：提供实例 ID、名称，或从候选列表中选择即可。"},
	}}
	eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{}}, func(string, map[string]any) bool {
		confirmCalls++
		return true
	})
	eng.SetInstanceOps(runner)
	require.NoError(t, eng.registry.SyncFromDescribe(map[string]any{
		"TotalCount": float64(2),
		"UHostSet": []any{
			map[string]any{"UHostId": "uhost-first", "Name": "image", "State": "Running"},
			map[string]any{"UHostId": "uhost-second", "Name": "train", "State": "Running"},
		},
	}, "test"))

	reply, err := eng.Chat(context.Background(), "实例开启后，镜像没办法正常运行", noopStep)

	require.NoError(t, err)
	require.Equal(t, "请告诉我要排查哪台实例：提供实例 ID、名称，或从候选列表中选择即可。", reply)
	require.Zero(t, runner.calls, "a vague symptom must return control to the agent before the SSH lane")
	require.Zero(t, confirmCalls, "the user must not see an authorization card for a self-elected list row")
	require.Len(t, model.calls, 2, "the blocked call is a normal observation, so the agent can ask for a target")
	secondRequest := model.calls[1]
	var sawSelectionBlock bool
	for _, msg := range secondRequest.Messages {
		if strings.Contains(msg.Content, "请先明确要排查的实例") {
			sawSelectionBlock = true
			break
		}
	}
	require.True(t, sawSelectionBlock, "the follow-up model call must receive the target-selection reason")
}

func TestInstanceOps_AllowsFreshUserSelectionButRejectsDifferentModelTarget(t *testing.T) {
	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "done"}}
	confirmCalls := 0
	eng := NewWithDeps(&mockLLM{}, &mockExecutor{results: map[string]map[string]any{}}, func(string, map[string]any) bool {
		confirmCalls++
		return true
	})
	eng.SetInstanceOps(runner)
	eng.turnContextViewThisTurn = AgentContext{
		CurrentQuestion: "帮我看看刚才那台",
		SelectedEntities: []SemanticEntityHint{{
			Kind: "instance", ID: "uhost-picked", Name: "picked", Source: SelectedInstanceSourceUser, Freshness: ContinuityFreshnessFresh,
		}},
	}
	eng.turnContextViewReady = true

	wrong := eng.executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", map[string]any{
		"UHostId": "uhost-other", "Task": "排查服务",
	}, noopStep)
	require.Zero(t, runner.calls)
	require.Zero(t, confirmCalls)
	require.Contains(t, wrong, "请先明确要排查的实例")

	right := eng.executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", map[string]any{
		"UHostId": "uhost-picked", "Task": "排查服务",
	}, noopStep)
	require.Equal(t, 1, runner.calls, "a fresh user selection must keep pronoun follow-ups working")
	require.Equal(t, 1, confirmCalls)
	require.True(t, strings.HasPrefix(right, finalReplyPrefix))
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

// An id that is not in the account gets the same honest, non-retryable treatment. This is the
// likeliest failure of all in practice — instance ids go stale fast (a test account replaced 7 of
// its 10 instances inside one hour on 2026-08-06) — and until it had its own branch the user was
// told 「请稍后重试，或到控制台查看实例状态」 about a box that no longer existed, which is advice
// that cannot work and points at the wrong layer. Same lesson as 门 8b above, third occurrence.
func TestInstanceOps_NotFoundRefusedHonestly(t *testing.T) {
	runner := &fakeInstanceOpsRunner{err: ErrInstanceOpsNotFound}
	eng := newInstanceOpsEngine(runner, alwaysConfirm)

	var steps []StepEvent
	out := eng.executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", instanceOpsArgs(), captureSteps(&steps))

	require.True(t, strings.HasPrefix(out, finalReplyPrefix), "a missing box is a terminal refusal")
	require.Contains(t, out, "找不到实例", "the refusal must name the real cause")
	require.Contains(t, out, "uhost-", "and the id the user asked about")
	require.NotContains(t, out, "请稍后重试", "retrying an id that is not in the account can never succeed")

	// A generic runner error must still keep the retry advice: only a well-formed response that
	// did not contain the id is non-retryable. Collapsing the two would make the new branch a
	// catch-all and lose the distinction it exists for.
	generic := &fakeInstanceOpsRunner{err: errors.New("sshops: audit begin failed")}
	out2 := newInstanceOpsEngine(generic, alwaysConfirm).
		executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", instanceOpsArgs(), noopStep)
	require.Contains(t, out2, "请稍后重试", "a transient failure keeps the retry advice")
	require.NotContains(t, out2, "找不到实例")
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
