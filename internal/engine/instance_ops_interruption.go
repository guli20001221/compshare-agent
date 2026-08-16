package engine

import (
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/observability"
)

// A diagnosis that ends without delivering its verdict leaves the user in front of a box that may
// have been changed, and until now the next thing they saw was an ordinary answer to whatever they
// asked next. The lane ships with allow_writes on: "8 ran, 1 refused" is not the question, "did it
// change anything, and what" is.
//
// This is the whole of the notice, and the boundaries are deliberate:
//
//   - It goes to the USER, as one StepEvent on the live activity stream. It is never appended to
//     e.messages, so the model neither sees it nor can restate it. TestInterruptionNoticeNeverEnters
//     TheModelsHistory pins that.
//   - It does not resume anything. There is no cursor here and no "skip what already ran"; the
//     harness that runs next starts from nothing, exactly as it does today.
//   - It never says a command did NOT run. What it lists is what the server OBSERVED settle before
//     the stream stopped, and the last command sent may have completed on the box with its result
//     never arriving. The closing sentence says so, and it is not optional decoration: a list
//     presented as complete would be the one way this notice could do harm.
//
// It is deliberately IN-MEMORY, on the session's Engine, and not read back from ssh_ops_audit. The
// case it serves is a browser disconnect, where the server process and the pooled Engine both
// survive and the user returns to the same SessionId. The case it misses is a process restart or an
// LRU/idle eviction — and in that case the durable row is not more informative either: a killed
// process never runs Finish, so the row is orphaned at 'started' with no steps at all, and
// ssh_ops_audit carries no session id to look one up by. Making this durable is a real change
// (a reader interface, a DB handle on a path that holds only InstanceOpsRunner, an index on
// request_uuid, and a decision about tenant-vs-session scoping), not a bigger version of this one.

// maxInterruptionNoticeCommands bounds the listed commands. A run that settled more than this before
// dying is already pathological; the count line still reports the true totals.
const maxInterruptionNoticeCommands = 20

// instanceOpsSettledStep is one command whose disposition the server actually OBSERVED. A command
// that was sent and never reported back is, by construction, absent — which is why the notice says
// its list may be incomplete rather than presenting it as the run's full extent.
type instanceOpsSettledStep struct {
	Command     string
	Tier        string // "read_only" | "mutating" | "destructive"; "" when the runner did not say
	Disposition string // "ran" | "refused" | "failed"
	Reason      string
}

// instanceOpsInterruption is the pending notice, stashed when a run ends without a verdict and
// drained by the next turn.
type instanceOpsInterruption struct {
	InstanceID string
	Steps      []instanceOpsSettledStep
}

// recordInstanceOpsInterruption stashes the notice. Called only when the run returned an error AND
// at least one command settled: a preflight failure (no SSH target, instance not found, not
// running, address unavailable) entered no box and has nothing to report.
func (e *Engine) recordInstanceOpsInterruption(instanceID string, steps []instanceOpsSettledStep) {
	if len(steps) == 0 {
		return
	}
	e.pendingInstanceOpsInterruption = &instanceOpsInterruption{
		InstanceID: instanceID,
		Steps:      append([]instanceOpsSettledStep(nil), steps...),
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
