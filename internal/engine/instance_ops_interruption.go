package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/opscontext"
	"github.com/compshare-agent/internal/security"
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

const (
	maxPersistedInstanceOpsJobPurposeRunes    = 200
	maxPersistedInstanceOpsJobInstanceIDRunes = 128
	maxPersistedInstanceOpsJobUpdatedAtBytes  = 64
)

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

// normalizePersistedInstanceOpsJobPurpose applies the conversation persistence
// secret/PII boundary, collapses whitespace and bounds by runes. The lifecycle
// protocol never supplies command text to this function.
func normalizePersistedInstanceOpsJobPurpose(purpose string) string {
	purpose = strings.Join(strings.Fields(security.RedactUserConversationText(purpose)), " ")
	runes := []rune(purpose)
	if len(runes) > maxPersistedInstanceOpsJobPurposeRunes {
		purpose = string(runes[:maxPersistedInstanceOpsJobPurposeRunes])
	}
	return purpose
}

func validPersistedInstanceOpsJob(job PersistedInstanceOpsJob) bool {
	instanceID := strings.TrimSpace(job.InstanceID)
	return instanceID != "" && len([]rune(instanceID)) <= maxPersistedInstanceOpsJobInstanceIDRunes &&
		validInstanceOpsJobID(job.JobID) &&
		activeInstanceOpsJobState(job.State)
}

func normalizePersistedInstanceOpsJob(job PersistedInstanceOpsJob) PersistedInstanceOpsJob {
	job.InstanceID = strings.TrimSpace(job.InstanceID)
	job.JobID = strings.TrimSpace(job.JobID)
	job.State = strings.TrimSpace(job.State)
	if !validPersistedInstanceOpsJob(job) {
		return PersistedInstanceOpsJob{}
	}
	job.Purpose = normalizePersistedInstanceOpsJobPurpose(job.Purpose)
	job.UpdatedAt = strings.TrimSpace(job.UpdatedAt)
	if len(job.UpdatedAt) > maxPersistedInstanceOpsJobUpdatedAtBytes {
		job.UpdatedAt = ""
	} else if job.UpdatedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, job.UpdatedAt); err != nil {
			job.UpdatedAt = ""
		}
	}
	return job
}

// observeInstanceOpsBackgroundJob updates the durable continuation cursor from
// one structured-tool event. Terminal observations clear the matching handle.
// Malformed lifecycle values are ignored, and no command text is retained.
// While A has an active cursor, a different job (including one on B) cannot
// silently replace it. This one-slot guarantee relies on the current singleton deployment plus
// agentpool's per-session lease. A deployment with multiple replicas must add a shared pre-launch
// reservation before it may rely on this cursor as an exclusive distributed slot.
func (e *Engine) observeInstanceOpsBackgroundJob(instanceID, jobID, state, purpose string) {
	if e == nil || !validInstanceOpsJobID(jobID) {
		return
	}
	instanceID = strings.TrimSpace(instanceID)
	jobID = strings.TrimSpace(jobID)
	state = strings.TrimSpace(state)
	if instanceID == "" {
		return
	}
	current := e.sessionState.PersistedInstanceOpsJob
	if activeInstanceOpsJobState(state) {
		if validPersistedInstanceOpsJob(current) &&
			(!strings.EqualFold(current.InstanceID, instanceID) || current.JobID != jobID) {
			return
		}
		purpose = normalizePersistedInstanceOpsJobPurpose(purpose)
		if purpose == "" && strings.EqualFold(current.InstanceID, instanceID) && current.JobID == jobID {
			purpose = current.Purpose
		}
		e.sessionState.PersistedInstanceOpsJob = PersistedInstanceOpsJob{
			InstanceID: instanceID,
			JobID:      jobID,
			State:      state,
			Purpose:    purpose,
			UpdatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		}
		e.sessionState.SchemaVersion = SessionStateSchemaCurrent
		return
	}
	if !terminalInstanceOpsJobState(state) {
		return
	}
	if validPersistedInstanceOpsJob(current) && strings.EqualFold(current.InstanceID, instanceID) && current.JobID == jobID {
		e.sessionState.PersistedInstanceOpsJob = PersistedInstanceOpsJob{}
		e.sessionState.SchemaVersion = SessionStateSchemaCurrent
	}
}

func (e *Engine) backgroundJobForInstance(instanceID string) *opscontext.BackgroundJob {
	current := e.sessionState.PersistedInstanceOpsJob
	if !validPersistedInstanceOpsJob(current) ||
		!strings.EqualFold(strings.TrimSpace(current.InstanceID), strings.TrimSpace(instanceID)) {
		return nil
	}
	job := opscontext.BackgroundJob{
		JobID:   current.JobID,
		State:   current.State,
		Purpose: normalizePersistedInstanceOpsJobPurpose(current.Purpose),
	}
	return &job
}

func (e *Engine) backgroundJobSlotBusyForOtherInstance(instanceID string) bool {
	if e == nil {
		return false
	}
	current := e.sessionState.PersistedInstanceOpsJob
	return validPersistedInstanceOpsJob(current) &&
		!strings.EqualFold(strings.TrimSpace(current.InstanceID), strings.TrimSpace(instanceID))
}

// clearBackgroundJobForInstance drops only the cursor whose control-plane target has been
// authoritatively reported absent. It cannot clear a different instance's in-flight work.
func (e *Engine) clearBackgroundJobForInstance(instanceID string) {
	if e == nil {
		return
	}
	current := e.sessionState.PersistedInstanceOpsJob
	if validPersistedInstanceOpsJob(current) &&
		strings.EqualFold(strings.TrimSpace(current.InstanceID), strings.TrimSpace(instanceID)) {
		e.sessionState.PersistedInstanceOpsJob = PersistedInstanceOpsJob{}
		e.sessionState.SchemaVersion = SessionStateSchemaCurrent
	}
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
	ran, mayModify, refused, failed := 0, 0, 0, 0
	for _, step := range notice.Steps {
		switch step.Disposition {
		case "ran":
			ran++
			if instanceOpsTierMayWrite(step.Tier) {
				mayModify++
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
		if mayModify > 0 {
			// Count every settled step, not only the bounded list rendered below.
			fmt.Fprintf(&b, "（其中 %d 条经确认执行，可能影响实例状态）", mayModify)
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

// instanceOpsTierMayWrite answers only for a tier the runner actually reported. An empty tier is
// UNKNOWN, never read_only: the two halves of the lane deploy separately, so a harness that predates
// the field is an ordinary rolling-upgrade state, and "we do not know whether this changed the box"
// must not print as "this did not change the box".
//
// A non-read-only tier means the command may affect state; it does not prove that it did.
func instanceOpsTierMayWrite(tier string) bool {
	return tier == "mutating" || tier == "destructive"
}

func interruptionStepLine(step instanceOpsSettledStep) string {
	cmd := truncateCommandForStep(step.Command)
	switch step.Disposition {
	case "ran":
		if instanceOpsTierMayWrite(step.Tier) {
			return fmt.Sprintf("已确认执行（可能影响实例状态）：`%s`", cmd)
		}
		return fmt.Sprintf("已执行：`%s`", cmd)
	case "refused":
		return fmt.Sprintf("未执行（%s）：`%s`", instanceOpsRefusalReason(step.Reason), cmd)
	default:
		// "failed" and anything unexpected. Deliberately NOT worded as "did not run": a command
		// that failed may have failed after changing something, and what the harness reports is the
		// transport's verdict, not the box's. A failed WRITE says so, because "did my instance get
		// modified" is the question and the honest answer there is "possibly".
		if instanceOpsTierMayWrite(step.Tier) {
			return fmt.Sprintf("执行未成功（可能已修改实例，结果不确定）：`%s`", cmd)
		}
		return fmt.Sprintf("执行未成功（结果不确定）：`%s`", cmd)
	}
}
