package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/compshare-agent/internal/llm"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

// The lane may execute confirmed repairs. A run killed mid-flight leaves the user in front of a box
// that may have been changed, and before this the next thing they saw was an ordinary answer to
// whatever they asked next. These pin what the notice says — and, more carefully, what it must
// never say.

func settled(cmd, tier, disposition string) instanceOpsSettledStep {
	return instanceOpsSettledStep{Command: cmd, Tier: tier, Disposition: disposition}
}

func TestInterruptionNoticeNamesConfirmedCommandsThatMayModify(t *testing.T) {
	msg := renderInstanceOpsInterruptionNotice(instanceOpsInterruption{
		InstanceID: "uhost-abc",
		Steps: []instanceOpsSettledStep{
			settled("df -h /", "read_only", "ran"),
			settled("rm -rf /root/.cache/pip", "mutating", "ran"),
			{Command: "systemctl restart vllm", Tier: "mutating", Disposition: "refused",
				Reason: "refused_confirmation_timeout"},
		},
	})

	require.Contains(t, msg, "uhost-abc")
	require.Contains(t, msg, "已确认执行 2 条命令")
	require.Contains(t, msg, "其中 1 条经确认执行，可能影响实例状态")
	require.Contains(t, msg, "拒绝 1 条")
	require.Contains(t, msg, "已确认执行（可能影响实例状态）：`rm -rf /root/.cache/pip`")
	require.Contains(t, msg, "已执行：`df -h /`")
	// The refused one carries WHY, from the same mapping the live stream uses — a card that expired
	// must not be reported to the user as a decision they made.
	require.Contains(t, msg, "systemctl restart vllm")
	require.NotContains(t, msg, "已执行：`systemctl restart vllm`")
}

// TestInterruptionNoticeNeverClaimsACommandDidNotRun is the honesty gate, and it is the reason this
// notice can exist at all. What the server lists is what it saw SETTLE; the last command sent may
// have completed on the box with its result never arriving. A list presented as exhaustive would
// tell a user that a half-finished repair did not happen.
func TestInterruptionNoticeNeverClaimsACommandDidNotRun(t *testing.T) {
	msg := renderInstanceOpsInterruptionNotice(instanceOpsInterruption{
		InstanceID: "uhost-abc",
		Steps: []instanceOpsSettledStep{
			settled("pip install torch", "mutating", "failed"),
		},
	})

	require.Contains(t, msg, "可能不完整")
	require.Contains(t, msg, "无法确认")
	// A "failed" command is NOT reported as one that did nothing: it may have failed after changing
	// something, and what the harness reports is the transport's verdict, not the box's. A failed
	// WRITE says so explicitly, because "did my instance get modified" is the question.
	require.Contains(t, msg, "执行未成功（可能已修改实例，结果不确定）：`pip install torch`")
	require.Contains(t, msg, "另有 1 条执行未成功")
	for _, forbidden := range []string{"未执行任何", "没有执行任何", "全部完成", "已全部"} {
		require.NotContains(t, msg, forbidden)
	}

	// ...and a failed READ does not acquire the write wording it has not earned.
	readMsg := renderInstanceOpsInterruptionNotice(instanceOpsInterruption{
		InstanceID: "uhost-abc",
		Steps:      []instanceOpsSettledStep{settled("nvidia-smi -q", "read_only", "failed")},
	})
	require.Contains(t, readMsg, "执行未成功（结果不确定）：`nvidia-smi -q`")
	require.NotContains(t, readMsg, "可能已修改实例")
}

func TestInterruptionNoticeReportsTrueTotalsWhenTheListIsTruncated(t *testing.T) {
	steps := make([]instanceOpsSettledStep, maxInterruptionNoticeCommands+7)
	for i := range steps {
		steps[i] = settled("ls /tmp", "read_only", "ran")
	}
	steps[0] = settled("systemctl stop vllm", "mutating", "ran")

	msg := renderInstanceOpsInterruptionNotice(instanceOpsInterruption{InstanceID: "uhost-x", Steps: steps})

	// The COUNTS are over every settled step, not over the listing — a cap on how much is printed
	// must not become a cap on what the user is told the run did.
	require.Contains(t, msg, "已确认执行 27 条命令")
	require.Contains(t, msg, "其中 1 条经确认执行，可能影响实例状态")
	require.Contains(t, msg, "另有 7 条未在此列出")
	require.Equal(t, maxInterruptionNoticeCommands+1, strings.Count(msg, "\n· "),
		"listed lines are bounded (plus the ellipsis line)")
}

// Nothing settled successfully — the run entered the box and got nowhere. Saying "0 条命令" with a
// list of nothing reads as a clean no-op, which it is not: the box was entered.
func TestInterruptionNoticeWithNothingSettledStillSaysTheBoxWasEntered(t *testing.T) {
	msg := renderInstanceOpsInterruptionNotice(instanceOpsInterruption{
		InstanceID: "uhost-x",
		Steps:      []instanceOpsSettledStep{settled("nvidia-smi", "read_only", "failed")},
	})
	require.Contains(t, msg, "没有正常结束")
	require.Contains(t, msg, "没有命令执行成功")
	require.Contains(t, msg, "无法确认")
	// The command is still LISTED. An early version took a short branch here and printed only the
	// summary sentence, which dropped the one line carrying "结果不确定" for the command whose
	// outcome is least knowable of all.
	require.Contains(t, msg, "`nvidia-smi`")
}

// A non-read-only tier is not proof that the command changed the instance.
func TestInterruptionNoticeSaysAConfirmedCommandMayModifyNotThatItDid(t *testing.T) {
	msg := renderInstanceOpsInterruptionNotice(instanceOpsInterruption{
		InstanceID: "uhost-x",
		Steps: []instanceOpsSettledStep{
			settled("sed -n '1,240p' /etc/nginx/nginx.conf", "mutating", "ran"),
		},
	})
	require.Contains(t, msg, "可能影响实例状态")
	require.NotContains(t, msg, "条修改了实例", "the tally must not assert the write happened")
	require.NotContains(t, msg, "已执行（修改了实例）", "nor may the per-command line")
}

// An unknown tier must never be read as read_only. The two halves of the lane deploy separately, so
// a harness that predates the field is a normal rolling-upgrade state — and "we do not know whether
// this changed the box" must not print as "this did not change the box".
func TestInterruptionNoticeTreatsAnUnknownTierAsUnknown(t *testing.T) {
	msg := renderInstanceOpsInterruptionNotice(instanceOpsInterruption{
		InstanceID: "uhost-x",
		Steps:      []instanceOpsSettledStep{settled("systemctl restart vllm", "", "ran")},
	})
	require.Contains(t, msg, "已确认执行 1 条命令")
	require.NotContains(t, msg, "可能修改了实例", "an unknown tier must not be counted into the write tally")
	require.NotContains(t, msg, "可能影响实例状态", "an unknown tier must not be labelled as state-changing")
	require.Contains(t, msg, "可能不完整")
}

func TestInterruptionNoticeIsStashedOnlyWhenSomethingSettled(t *testing.T) {
	e := &Engine{}
	e.recordInstanceOpsInterruption("uhost-x", nil)
	require.Nil(t, e.pendingInstanceOpsInterruption,
		"a preflight failure entered no box and has nothing to report")

	e.recordInstanceOpsInterruption("uhost-x", []instanceOpsSettledStep{settled("df -h", "read_only", "ran")})
	require.NotNil(t, e.pendingInstanceOpsInterruption)
}

func TestInstanceOpsInterruptionSummaryIsReadOnlyAndUsesCurrentTurn(t *testing.T) {
	var missing *Engine
	require.Empty(t, missing.InstanceOpsInterruptionSummary())
	e := &Engine{}
	require.Empty(t, e.InstanceOpsInterruptionSummary())
	e.observeInstanceOpsBackgroundJob("uhost-x", "job-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "running", "安装依赖")
	e.recordInstanceOpsInterruption("uhost-x", []instanceOpsSettledStep{
		settled("systemctl start app", "mutating", "ran"),
	})
	pending := e.pendingInstanceOpsInterruption
	state := e.sessionState
	summary := e.InstanceOpsInterruptionSummary()
	require.Contains(t, summary, "本轮对实例 uhost-x")
	require.Contains(t, summary, "已确认执行 1 条命令")
	require.Contains(t, summary, "不会重新启动")
	require.Equal(t, summary, e.InstanceOpsInterruptionSummary())
	require.Same(t, pending, e.pendingInstanceOpsInterruption, "reading a persistence summary must not consume the next-turn notice")
	require.Equal(t, state, e.sessionState, "rendering must not alter job/session continuation state")
	var emitted []StepEvent
	e.emitPendingInstanceOpsInterruption(func(event StepEvent) { emitted = append(emitted, event) })
	require.Len(t, emitted, 1)
	require.Contains(t, emitted[0].Message, "上一轮对实例 uhost-x")
	require.Empty(t, e.InstanceOpsInterruptionSummary())
}

// TestInterruptionNoticeIsDeliveredOnceAndOnlyOnce: an interrupted run that narrated itself on every
// subsequent turn would be worse than silence — the user cannot tell a fresh interruption from an
// old one being replayed.
func TestInterruptionNoticeIsDeliveredOnceAndOnlyOnce(t *testing.T) {
	e := &Engine{}
	e.recordInstanceOpsInterruption("uhost-x", []instanceOpsSettledStep{
		settled("rm -rf /root/.cache/pip", "mutating", "ran"),
	})

	var emitted []StepEvent
	collect := func(ev StepEvent) { emitted = append(emitted, ev) }

	e.emitPendingInstanceOpsInterruption(collect)
	require.Len(t, emitted, 1)
	require.Equal(t, instanceOpsInterruptionAction, emitted[0].Action)
	// StepUserNotice, not StepBlocked. Nothing was called, failed or blocked on this turn —
	// the run it describes ended on a previous one — and the trace recorder keys off exactly
	// this type to stay out of the tool counters (TestChatTraceRecorder_UserNoticeIsNotAToolEvent).
	require.Equal(t, StepUserNotice, emitted[0].Type)
	require.Contains(t, emitted[0].Message, "rm -rf /root/.cache/pip")

	e.emitPendingInstanceOpsInterruption(collect)
	require.Len(t, emitted, 1, "the notice must be drained, not repeated every turn")
}

// TestInterruptionNoticeNeverEntersTheModelsHistory is the boundary the whole design rests on. The
// notice is for the USER: the model must not see it, restate it, summarize it, or treat it as an
// instruction to continue anything. It travels on the activity stream only.
func TestInterruptionNoticeNeverEntersTheModelsHistory(t *testing.T) {
	e := &Engine{}
	e.messages = []openai.ChatCompletionMessage{{Role: "system", Content: "sys"}}
	before := len(e.messages)

	e.recordInstanceOpsInterruption("uhost-x", []instanceOpsSettledStep{
		settled("rm -rf /root/.cache/pip", "mutating", "ran"),
	})
	var emitted []StepEvent
	e.emitPendingInstanceOpsInterruption(func(ev StepEvent) { emitted = append(emitted, ev) })

	require.Len(t, emitted, 1, "the user is told")
	require.Len(t, e.messages, before, "...and the model is not")
	for _, m := range e.messages {
		require.NotContains(t, m.Content, "rm -rf /root/.cache/pip")
		require.NotContains(t, m.Content, "上一轮")
	}
}

// A nil onStep (a caller with no activity stream) must drain the notice rather than hold it for a
// later turn that might have one — holding it would surface a stale interruption at an arbitrary
// point, which is exactly the "when did this happen?" confusion the notice exists to remove.
func TestInterruptionNoticeIsDrainedEvenWithNoStreamToDeliverItOn(t *testing.T) {
	e := &Engine{}
	e.recordInstanceOpsInterruption("uhost-x", []instanceOpsSettledStep{settled("df -h", "read_only", "ran")})
	e.emitPendingInstanceOpsInterruption(nil)
	require.Nil(t, e.pendingInstanceOpsInterruption)
}

// TestAKilledRunStashesWhatItSawForTheNextTurn closes the loop the unit tests above leave open: the
// dispatch path must actually accumulate what it watched settle and hand it over. Note the run here
// emits MORE commands than the live activity stream will show (maxInstanceOpsStepEvents), which is
// the case the accumulation exists to survive — a cap on the UI feed must not become a cap on what
// the user is told a killed run did.
func TestAKilledRunStashesWhatItSawForTheNextTurn(t *testing.T) {
	exit := 0
	progress := []InstanceOpsProgress{
		{Kind: InstanceOpsProgressConnected},
		{Kind: InstanceOpsProgressCommand, Command: "df -h /", Tier: "read_only", Disposition: "ran", ExitCode: &exit},
		{Kind: InstanceOpsProgressCommand, Command: "rm -rf /root/.cache/pip", Tier: "mutating", Disposition: "ran", ExitCode: &exit},
		{Kind: InstanceOpsProgressCommand, Command: "systemctl restart vllm", Tier: "mutating",
			Disposition: "refused", Reason: "refused_client_disconnect"},
	}
	// Padded PAST the activity-stream cap on purpose. That cap is a UI budget; accumulating behind
	// it instead of in front of it would silently turn "how much the console shows" into "how much
	// the user is told a killed run did". With 3 commands the two are indistinguishable.
	for i := 0; i < maxInstanceOpsStepEvents; i++ {
		progress = append(progress, InstanceOpsProgress{
			Kind: InstanceOpsProgressCommand, Command: fmt.Sprintf("ls /tmp/%d", i),
			Tier: "read_only", Disposition: "ran", ExitCode: &exit,
		})
	}
	runner := &fakeInstanceOpsRunner{progress: progress, err: context.Canceled}
	eng := newInstanceOpsEngine(runner, alwaysConfirm)

	var steps []StepEvent
	eng.executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", instanceOpsArgs(), captureSteps(&steps))

	require.NotNil(t, eng.pendingInstanceOpsInterruption, "a killed run must leave the user an account of it")
	require.Equal(t, "uhost-1", eng.pendingInstanceOpsInterruption.InstanceID)
	require.Len(t, eng.pendingInstanceOpsInterruption.Steps, 3+maxInstanceOpsStepEvents,
		"every settled command is accumulated, including the ones past the activity-stream cap")
	require.Equal(t, "mutating", eng.pendingInstanceOpsInterruption.Steps[1].Tier,
		"the tier has to survive the adapter, or a write cannot be told from a read")

	// It is NOT delivered on the turn that stashed it — the drain runs at turn entry.
	for _, s := range steps {
		require.NotEqual(t, instanceOpsInterruptionAction, s.Action)
	}

	var next []StepEvent
	eng.emitPendingInstanceOpsInterruption(captureSteps(&next))
	require.Len(t, next, 1)
	require.Contains(t, next[0].Message, "其中 1 条经确认执行，可能影响实例状态")
}

// A run that never entered the box leaves nothing behind. Every preflight failure (no SSH target,
// instance not found, not running, address unavailable) lands here, and a notice for one of them
// would tell a user their instance had been touched when it had not.
func TestAPreflightFailureStashesNothing(t *testing.T) {
	runner := &fakeInstanceOpsRunner{err: ErrInstanceOpsNoSSHTarget}
	eng := newInstanceOpsEngine(runner, alwaysConfirm)

	var steps []StepEvent
	eng.executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", instanceOpsArgs(), captureSteps(&steps))

	require.Nil(t, eng.pendingInstanceOpsInterruption)
}

// A run that DELIVERED its verdict is not an interruption. The user already read what happened.
func TestACompletedRunStashesNothing(t *testing.T) {
	exit := 0
	runner := &fakeInstanceOpsRunner{
		progress: []InstanceOpsProgress{
			{Kind: InstanceOpsProgressCommand, Command: "df -h /", Tier: "read_only", Disposition: "ran", ExitCode: &exit},
		},
		verdict: InstanceOpsVerdict{Text: "磁盘已满", Ran: 1},
	}
	eng := newInstanceOpsEngine(runner, alwaysConfirm)

	var steps []StepEvent
	eng.executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", instanceOpsArgs(), captureSteps(&steps))

	require.Nil(t, eng.pendingInstanceOpsInterruption)
}

// TestTheNextRealTurnDeliversTheNotice pins the WIRING, which every test above leaves untested by
// calling the drain directly. Removing the one line at the top of ChatWithOptions leaves a notice
// that is stashed correctly, rendered correctly, and never shown to anyone.
func TestTheNextRealTurnDeliversTheNotice(t *testing.T) {
	model := &mockLLM{responses: []llm.ChatResponse{{Content: "好的。"}}}
	eng := NewWithDeps(model, &mockExecutor{results: map[string]map[string]any{}}, nil)
	eng.recordInstanceOpsInterruption("uhost-1", []instanceOpsSettledStep{
		settled("rm -rf /root/.cache/pip", "mutating", "ran"),
	})

	var steps []StepEvent
	_, err := eng.Chat(context.Background(), "刚才怎么了", captureSteps(&steps))
	require.NoError(t, err)

	var notices []StepEvent
	for _, s := range steps {
		if s.Action == instanceOpsInterruptionAction {
			notices = append(notices, s)
		}
	}
	require.Len(t, notices, 1, "the next turn must deliver the notice on the activity stream")
	require.Equal(t, StepUserNotice, notices[0].Type,
		"a turn that merely REPORTS a previous interruption must not trace as a blocked tool")
	require.Contains(t, notices[0].Message, "rm -rf /root/.cache/pip")
	require.Nil(t, eng.pendingInstanceOpsInterruption)

	// ...and the turn after that is silent.
	var again []StepEvent
	_, err = eng.Chat(context.Background(), "好的", captureSteps(&again))
	require.NoError(t, err)
	for _, s := range again {
		require.NotEqual(t, instanceOpsInterruptionAction, s.Action)
	}
}

func TestBackgroundJobSurvivesAnInterruptedTurnAndOnlyPollsOnTheSameInstance(t *testing.T) {
	jobID := "job-" + strings.Repeat("a", 32)
	runner := &fakeInstanceOpsRunner{
		progress: []InstanceOpsProgress{{
			Kind: InstanceOpsProgressBackgroundJob, JobID: jobID, JobState: "unknown",
			JobPurpose: " 下载 LoRA   权重 ",
		}},
		err: context.Canceled,
	}
	eng := newInstanceOpsEngine(runner, alwaysConfirm)

	eng.executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", instanceOpsArgs(), func(StepEvent) {})

	job := eng.sessionState.PersistedInstanceOpsJob
	require.Equal(t, "uhost-1", job.InstanceID)
	require.Equal(t, jobID, job.JobID)
	require.Equal(t, "下载 LoRA 权重", job.Purpose)
	require.NotEmpty(t, job.UpdatedAt)
	require.NotNil(t, eng.pendingInstanceOpsInterruption)
	require.Contains(t, renderInstanceOpsInterruptionNotice(*eng.pendingInstanceOpsInterruption), jobID)
	require.Nil(t, eng.backgroundJobForInstance("uhost-other"),
		"an opaque handle must never be offered to a different instance")

	// The next turn on the same instance gets only the opaque handle. No command is retained in the
	// continuation payload, so the harness can poll but cannot replay what start_background_job ran.
	runner.progress = []InstanceOpsProgress{{
		Kind: InstanceOpsProgressCommand, Command: "poll_background_job job=" + jobID,
		Tier: "read_only", Disposition: "ran", JobID: jobID, JobState: "succeeded",
	}}
	runner.err = nil
	runner.verdict = InstanceOpsVerdict{Text: "后台任务已完成", Ran: 1}
	eng.instanceOpsRanThisTurn = false
	eng.executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", instanceOpsArgs(), func(StepEvent) {})

	require.NotNil(t, runner.lastReq.Context.PendingBackgroundJob)
	require.Equal(t, jobID, runner.lastReq.Context.PendingBackgroundJob.JobID)
	require.Equal(t, "unknown", runner.lastReq.Context.PendingBackgroundJob.State)
	require.Equal(t, "下载 LoRA 权重", runner.lastReq.Context.PendingBackgroundJob.Purpose)
	require.True(t, eng.sessionState.PersistedInstanceOpsJob.IsZero(),
		"a terminal poll must clear the live handle instead of polling it forever")
}

func TestBackgroundJobUnknownStateIsPolledButMalformedStateCannotClearIt(t *testing.T) {
	jobID := "job-" + strings.Repeat("b", 32)
	eng := &Engine{}
	eng.observeInstanceOpsBackgroundJob("uhost-1", jobID, "unknown", "安装依赖")
	require.Equal(t, "unknown", eng.backgroundJobForInstance("uhost-1").State)

	eng.observeInstanceOpsBackgroundJob("uhost-1", jobID, "future_state", "")
	require.NotNil(t, eng.backgroundJobForInstance("uhost-1"),
		"version skew must degrade to keeping an unresolved handle, not silently dropping it")

	eng.observeInstanceOpsBackgroundJob("uhost-1", jobID, "not_found", "")
	require.Nil(t, eng.backgroundJobForInstance("uhost-1"))

	// This is deliberately one slot rather than a job registry. An active job on
	// A cannot be silently replaced by a later event for B.
	eng.observeInstanceOpsBackgroundJob("uhost-1", jobID, "running", "安装依赖")
	newJobID := "job-" + strings.Repeat("d", 32)
	eng.observeInstanceOpsBackgroundJob("uhost-2", newJobID, "unknown", "另一项任务")
	require.Equal(t, jobID, eng.backgroundJobForInstance("uhost-1").JobID)
	require.Nil(t, eng.backgroundJobForInstance("uhost-2"))
}

func TestBackgroundJobOnAnotherInstanceMakesTheSingleDurableSlotBusy(t *testing.T) {
	jobID := "job-" + strings.Repeat("a", 32)
	runner := &fakeInstanceOpsRunner{verdict: InstanceOpsVerdict{Text: "只读排查完成"}}
	eng := newInstanceOpsEngine(runner, alwaysConfirm)
	eng.observeInstanceOpsBackgroundJob("uhost-1", jobID, "running", "下载模型")
	eng.lastUserMsg = "请排查 uhost-2"
	eng.turnContextViewThisTurn = AgentContext{CurrentQuestion: eng.lastUserMsg}
	eng.turnContextViewReady = true

	eng.executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", map[string]any{
		"UHostId": "uhost-2", "Task": "排查服务", "Mode": "repair",
	}, func(StepEvent) {})

	require.Equal(t, 1, runner.calls)
	require.Nil(t, runner.lastReq.Context.PendingBackgroundJob,
		"another instance must never receive the opaque handle")
	require.True(t, runner.lastReq.Context.BackgroundJobSlotBusy,
		"the harness must refuse a second background launch instead of executing an untrackable job")
	require.Equal(t, jobID, eng.sessionState.PersistedInstanceOpsJob.JobID)
}

func TestNotFoundClearsMatchingBackgroundJobAndReleasesSlot(t *testing.T) {
	jobID := "job-" + strings.Repeat("f", 32)
	runner := &fakeInstanceOpsRunner{err: ErrInstanceOpsNotFound}
	eng := newInstanceOpsEngine(runner, alwaysConfirm)
	eng.observeInstanceOpsBackgroundJob("uhost-gone", jobID, "running", "下载模型")
	eng.lastUserMsg = "检查已经释放的 uhost-gone"
	eng.turnContextViewThisTurn = AgentContext{CurrentQuestion: eng.lastUserMsg}
	eng.turnContextViewReady = true

	eng.executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", map[string]any{
		"UHostId": "uhost-gone", "Task": "检查后台任务", "Mode": "repair",
	}, func(StepEvent) {})

	require.True(t, eng.sessionState.PersistedInstanceOpsJob.IsZero())
	require.Nil(t, eng.pendingInstanceOpsInterruption,
		"NotFound before any settled command must not promise that an unpollable job was retained")

	runner.err = nil
	runner.verdict = InstanceOpsVerdict{Text: "另一实例可以继续"}
	eng.instanceOpsRanThisTurn = false
	eng.lastUserMsg = "排查 uhost-next"
	eng.turnContextViewThisTurn = AgentContext{CurrentQuestion: eng.lastUserMsg}
	eng.turnContextViewReady = true
	eng.executeInstanceOps(context.Background(), "DiagnoseInstanceInternals", map[string]any{
		"UHostId": "uhost-next", "Task": "排查服务", "Mode": "repair",
	}, func(StepEvent) {})

	require.False(t, runner.lastReq.Context.BackgroundJobSlotBusy)
}

func TestBackgroundJobPurposeIsRedactedAndRuneBounded(t *testing.T) {
	jobID := "job-" + strings.Repeat("e", 32)
	eng := &Engine{}
	eng.observeInstanceOpsBackgroundJob("uhost-1", jobID, "running",
		"联系 user@example.com token=secret-value "+strings.Repeat("长", 240))
	purpose := eng.sessionState.PersistedInstanceOpsJob.Purpose
	require.LessOrEqual(t, len([]rune(purpose)), maxPersistedInstanceOpsJobPurposeRunes)
	require.NotContains(t, purpose, "user@example.com")
	require.NotContains(t, purpose, "secret-value")
}

func TestBackgroundJobRoundTripsAcrossEngineRebuildWithoutCommand(t *testing.T) {
	jobID := "job-" + strings.Repeat("c", 32)
	hot := &Engine{sessionStateHydrated: true, sessionStateVersion: 7,
		sessionState: SessionState{SchemaVersion: SessionStateSchemaV7}}
	hot.observeInstanceOpsBackgroundJob("uhost-1", jobID, "running", "下载 token=secret-value 模型")
	state, version, hydrated := hot.SessionStateSnapshot()
	require.True(t, hydrated)
	require.Equal(t, SessionStateSchemaCurrent, state.SchemaVersion)

	raw, err := json.Marshal(PersistedContext{AgentSessionState: state})
	require.NoError(t, err)
	require.NotContains(t, string(raw), "command")
	require.NotContains(t, string(raw), "secret-value")

	persisted, err := ParsePersistedContext(raw)
	require.NoError(t, err)
	cold := &Engine{}
	cold.RehydrateHistory(nil)
	cold.SetSessionState(persisted.AgentSessionState, version+1)
	resumed := cold.backgroundJobForInstance("uhost-1")
	require.NotNil(t, resumed)
	require.Equal(t, jobID, resumed.JobID)
	require.Contains(t, resumed.Purpose, "[REDACTED]")
	require.Nil(t, cold.backgroundJobForInstance("uhost-2"))

	// A whole-session reset clears the slot; normal HTTP hydration restores it
	// by calling SetSessionState after this boundary.
	cold.InitWithContext("another isolated session")
	require.Nil(t, cold.backgroundJobForInstance("uhost-1"))
}
