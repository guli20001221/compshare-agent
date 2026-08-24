package engine

import (
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/opscontext"
)

// An interrupted diagnosis may already have changed the instance. The next turn
// therefore receives one user-only activity notice containing only steps whose
// outcome the server observed. It is not model history and never resumes work.
// The notice is intentionally session-memory-only; process restart and eviction
// simply lose it rather than guessing from an incomplete audit row.

// maxInterruptionNoticeCommands bounds the listed commands. A run that settled more than this before
// dying is already pathological; the count line still reports the true totals.
const maxInterruptionNoticeCommands = 20

// instanceOpsSettledStep is one command whose disposition the server observed.
// A sent command whose result never returned is absent, so the notice must say
// that its list may be incomplete.
type instanceOpsSettledStep struct {
	Command     string
	Tier        string // "read_only" | "mutating" | "destructive"; "" when the runner did not say
	Disposition string // "ran" | "refused" | "failed"
	Reason      string
}

// instanceOpsInterruption is the pending notice, stashed when a run ends without a verdict and
// drained by the next turn.
type instanceOpsInterruption struct {
	InstanceID    string
	Steps         []instanceOpsSettledStep
	BackgroundJob *opscontext.BackgroundJob
}

// instanceOpsBackgroundJob binds an opaque guest handle to the box that produced it. It is kept
// outside SessionState on purpose: this is continuity for one still-live process/session, not a
// retired durable turn or a command replay record.
type instanceOpsBackgroundJob struct {
	InstanceID string
	Job        opscontext.BackgroundJob
}

func validInstanceOpsJobID(id string) bool {
	id = strings.TrimSpace(id)
	if len(id) != len("job-")+32 || id[:len("job-")] != "job-" {
		return false
	}
	for _, r := range id[len("job-"):] {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func activeInstanceOpsJobState(state string) bool {
	switch strings.TrimSpace(state) {
	case "started", "running", "unknown":
		return true
	default:
		return false
	}
}

func terminalInstanceOpsJobState(state string) bool {
	switch strings.TrimSpace(state) {
	case "succeeded", "failed", "interrupted", "not_found":
		return true
	default:
		return false
	}
}

// observeInstanceOpsBackgroundJob updates the live-session continuation handle from one settled
// structured-tool event. Terminal observations clear the matching handle. Malformed lifecycle
// values are ignored, and no command text is retained.
func (e *Engine) observeInstanceOpsBackgroundJob(instanceID, jobID, state string) {
	if e == nil || !validInstanceOpsJobID(jobID) {
		return
	}
	instanceID = strings.TrimSpace(instanceID)
	jobID = strings.TrimSpace(jobID)
	state = strings.TrimSpace(state)
	if activeInstanceOpsJobState(state) {
		e.pendingInstanceOpsBackgroundJob = &instanceOpsBackgroundJob{
			InstanceID: instanceID,
			Job:        opscontext.BackgroundJob{JobID: jobID, State: state},
		}
		return
	}
	if !terminalInstanceOpsJobState(state) {
		return
	}
	if current := e.pendingInstanceOpsBackgroundJob; current != nil &&
		strings.EqualFold(current.InstanceID, instanceID) && current.Job.JobID == jobID {
		e.pendingInstanceOpsBackgroundJob = nil
	}
}

func (e *Engine) backgroundJobForInstance(instanceID string) *opscontext.BackgroundJob {
	current := e.pendingInstanceOpsBackgroundJob
	if current == nil || !strings.EqualFold(strings.TrimSpace(current.InstanceID), strings.TrimSpace(instanceID)) {
		return nil
	}
	job := current.Job
	return &job
}

// recordInstanceOpsInterruption stashes the notice when the run returned an error and either a
// command settled or an approved background launch published its opaque handle. A preflight
// failure has neither and remains silent.
func (e *Engine) recordInstanceOpsInterruption(instanceID string, steps []instanceOpsSettledStep) {
	job := e.backgroundJobForInstance(instanceID)
	if len(steps) == 0 && job == nil {
		return
	}
	e.pendingInstanceOpsInterruption = &instanceOpsInterruption{
		InstanceID:    instanceID,
		Steps:         append([]instanceOpsSettledStep(nil), steps...),
		BackgroundJob: job,
	}
}

// emitPendingInstanceOpsInterruption delivers the notice at the start of the next turn and clears
// it. Draining unconditionally — even if rendering produced nothing — is what stops one interrupted
// run from narrating itself on every turn that follows.
func (e *Engine) emitPendingInstanceOpsInterruption(onStep func(StepEvent)) {
	pending := e.pendingInstanceOpsInterruption
	e.pendingInstanceOpsInterruption = nil
	if pending == nil || onStep == nil {
		return
	}
	if msg := renderInstanceOpsInterruptionNotice(*pending); msg != "" {
		onStep(StepEvent{
			// StepUserNotice, NOT StepBlocked. Nothing was called, failed or blocked on this
			// turn — the run it describes ended on a previous one. Sending it as StepBlocked
			// made the trace record a phantom blocked tool error, so the next turn after an
			// interruption looked like a turn where a tool had been refused.
			Type:    StepUserNotice,
			Action:  instanceOpsInterruptionAction,
			Source:  observability.ToolSourceDiagnosisInternal,
			Message: msg,
		})
	}
}

// instanceOpsInterruptionAction is the step frame's Action. It is NOT a tool name — nothing
// dispatches on it — and it exists so the console can label and style the frame; step_label.go
// carries its Chinese label.
const instanceOpsInterruptionAction = "InstanceOpsInterrupted"

// renderInstanceOpsInterruptionNotice builds the user-facing text. Deterministic: same input, same
// bytes, no model involved.
func renderInstanceOpsInterruptionNotice(notice instanceOpsInterruption) string {
	ran, wrote, refused, failed := 0, 0, 0, 0
	for _, step := range notice.Steps {
		switch step.Disposition {
		case "ran":
			ran++
			if isInstanceOpsWriteTier(step.Tier) {
				wrote++
			}
		case "refused":
			refused++
		default:
			failed++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "上一轮对实例 %s 的实例内排查没有正常结束。", notice.InstanceID)
	if ran > 0 {
		fmt.Fprintf(&b, "中断前已确认执行 %d 条命令", ran)
		if wrote > 0 {
			// The one number that decides what the user does next. It is counted over every settled
			// step, not over the listing, so a truncated list cannot understate it.
			fmt.Fprintf(&b, "（其中 %d 条修改了实例）", wrote)
		}
	} else {
		fmt.Fprint(&b, "中断前没有命令执行成功")
	}
	if refused > 0 {
		fmt.Fprintf(&b, "，拒绝 %d 条", refused)
	}
	if failed > 0 {
		// Counted and listed separately rather than folded into either side. A failed command is
		// the case where "did it change anything" is least knowable — a write that timed out may
		// have half-landed — so it must not be reported as a run, nor quietly dropped.
		fmt.Fprintf(&b, "，另有 %d 条执行未成功", failed)
	}
	b.WriteString("：\n")

	listed := notice.Steps
	if len(listed) > maxInterruptionNoticeCommands {
		listed = listed[:maxInterruptionNoticeCommands]
	}
	for _, step := range listed {
		b.WriteString("· " + interruptionStepLine(step) + "\n")
	}
	if omitted := len(notice.Steps) - len(listed); omitted > 0 {
		fmt.Fprintf(&b, "· ……另有 %d 条未在此列出\n", omitted)
	}
	b.WriteString(interruptionIncompletenessNotice)
	if notice.BackgroundJob != nil {
		if notice.BackgroundJob.State == "unknown" {
			fmt.Fprintf(&b, " 后台任务可能已经启动，任务编号为 %s；继续排查同一实例时只会查询该任务状态，不会重新启动。",
				notice.BackgroundJob.JobID)
		} else {
			fmt.Fprintf(&b, " 后台任务 %s 已保留；继续排查同一实例时只会查询该任务状态，不会重新启动。",
				notice.BackgroundJob.JobID)
		}
	}
	return b.String()
}

// interruptionIncompletenessNotice is the sentence that keeps this notice honest. The server lists
// what it saw SETTLE; a command whose result never came back is absent from that list and may still
// have run to completion on the box. Presenting the list as exhaustive would tell a user that a
// half-finished repair did not happen.
const interruptionIncompletenessNotice = "以上是中断前已回传结果的命令，可能不完整：" +
	"最后发出的命令是否已在实例上执行完成，无法确认。"

// isInstanceOpsWriteTier answers only for a tier the runner actually reported. An empty tier is
// UNKNOWN, never read_only: the two halves of the lane deploy separately, so a harness that predates
// the field is an ordinary rolling-upgrade state, and "we do not know whether this changed the box"
// must not print as "this did not change the box".
func isInstanceOpsWriteTier(tier string) bool {
	return tier == "mutating" || tier == "destructive"
}

func interruptionStepLine(step instanceOpsSettledStep) string {
	cmd := truncateCommandForStep(step.Command)
	switch step.Disposition {
	case "ran":
		if isInstanceOpsWriteTier(step.Tier) {
			return fmt.Sprintf("已执行（修改了实例）：`%s`", cmd)
		}
		return fmt.Sprintf("已执行：`%s`", cmd)
	case "refused":
		return fmt.Sprintf("未执行（%s）：`%s`", instanceOpsRefusalReason(step.Reason), cmd)
	default:
		// "failed" and anything unexpected. Deliberately NOT worded as "did not run": a command
		// that failed may have failed after changing something, and what the harness reports is the
		// transport's verdict, not the box's. A failed WRITE says so, because "did my instance get
		// modified" is the question and the honest answer there is "possibly".
		if isInstanceOpsWriteTier(step.Tier) {
			return fmt.Sprintf("执行未成功（可能已修改实例，结果不确定）：`%s`", cmd)
		}
		return fmt.Sprintf("执行未成功（结果不确定）：`%s`", cmd)
	}
}
