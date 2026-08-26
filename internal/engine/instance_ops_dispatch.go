package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/security"
	"github.com/compshare-agent/internal/tools"
)

// maxInstanceOpsStepEvents caps the per-command activity events the engine emits
// for one in-instance diagnosis. It mirrors the harness's own step cap
// (maxHarnessSteps=50) and the per-step audit write it drives, so a
// runaway harness cannot flood the activity stream or the turn's DB writes. Beyond
// the cap, per-command events stop; the terminal summary still reports the totals.
const maxInstanceOpsStepEvents = 50

// Target refusal text is split by audience: the activity stream gives the user
// an actionable instruction, while the tool result explains the binding rule to
// the model. A bare confirmation or a console-only selection does not identify a
// target in this conversation.
const (
	// instanceOpsTargetRefusalForUser is what the person reads. Actionable, no internal policy.
	instanceOpsTargetRefusalForUser = "尚未开始实例内排查。请直接回复要排查的实例 ID 或实例名称；" +
		"如果上面列出了候选实例，回复对应的序号也可以。"
	// instanceOpsTargetRefusalForModel rides the tool result, which the user never sees. It states
	// the constraint AND the exits, including the one the agent kept getting wrong: a bare 「确认」
	// is not an identifier and re-asking for it loops.
	instanceOpsTargetRefusalForModel = "请先明确要排查的实例。只有用户在消息中直接给出的实例 ID、" +
		"唯一实例名，或对已展示候选列表的序号，才能作为本次排查目标；不能自行从实例列表挑选，" +
		"也不能把仅仅读到过的实例当作目标。请让用户在下一条消息里直接给出实例 ID 或名称——" +
		"用户回复「确认」「是的」这类不含标识的内容不会解除该限制，在控制台里选中实例也不会。"
)

// executeInstanceOps handles a DiagnoseInstanceInternals tool call: the
// confirmation-gated in-instance diagnosis and repair lane. It is dispatched from executeToolOnce BEFORE the
// diagnosis-chain and mutating-tool branches, so it never inherits the
// SafeToolExecutor per-attempt wall-clock ceiling. The whole lane is inert unless
// a runner was wired (server SharedDeps.InstanceOps; tests may use SetInstanceOps) — when
// e.instanceOps is nil the tool is not even in the model's window (see
// centralAgentToolWindow), and this method fails closed on the off chance the model
// names it anyway.
//
// Ordering:
//   - nil-runner  → feature disabled, inert refusal, no slot consumed (INV-10)
//   - param check → UHostId + Task required
//   - target proof → the user named/selected this exact instance (never pick a list row)
//   - INV-11 gate → at most one in-instance run per turn; SET before confirm so a
//     declined card still spends the slot (repeated cards would harass the user)
//   - INV-7       → e.confirmFn == nil fails closed (never fetch, never spawn)
//   - confirm     → user authorizes on the card; a decline continues the turn
//   - Run         → progress→StepEvent live; verdict → finalReplyPrefix (unrewritable)
func (e *Engine) executeInstanceOps(ctx context.Context, action string, args map[string]any, onStep func(StepEvent)) string {
	// INV-10: with no runner the lane is off. The tool is absent from the window,
	// so a well-behaved model cannot reach here; a replayed/hallucinated call gets
	// an inert refusal and does NOT consume the per-turn slot.
	if e.instanceOps == nil {
		msg := "实例内排查功能当前不可用。"
		onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: msg})
		return friendlyToolResultJSON(msg)
	}

	filtered := e.safeExecutor.FilterArgs(action, args)
	instanceIDArg, _ := filtered["UHostId"].(string)
	taskArg, _ := filtered["Task"].(string)
	instanceID := strings.TrimSpace(instanceIDArg)
	task := strings.TrimSpace(taskArg)
	if instanceID == "" || task == "" {
		msg := "请提供要排查的实例 ID 和本次排查目标。"
		onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: msg})
		return friendlyToolResultJSON(msg)
	}
	// Build the local-only portion before the entry card so a planner that copied
	// a user Authorization value into Task cannot expose it on that card. The
	// private values stay json:"-" and are handed to the runner only after the
	// user approves entry; this step performs no platform call or SSH access.
	modelContext := e.instanceOpsModelContext()
	authorizations := make([]string, 0, len(modelContext.ProbeAuthorizations))
	for _, item := range modelContext.ProbeAuthorizations {
		authorizations = append(authorizations, item.Value)
	}
	task = security.RedactKnownAuthorizationText(task, authorizations)
	// A model-authored id is not selection proof. Reuse the write-path binding
	// rules; an expired user selection may reach a new card for the same id but is
	// never authority by itself.
	if !e.instanceOpsTargetMayReachConfirmation(instanceID) {
		onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: instanceOpsTargetRefusalForUser})
		return friendlyToolResultJSON(instanceOpsTargetRefusalForModel)
	}
	// A cold registry can make a literal ID provable only at this gate. Record the
	// user's designation before confirmation so a declined or timed-out card does
	// not erase it. Account-single and expired-selection fallbacks are not new user
	// designations and therefore are not recorded here.
	if e.userNamedInstanceThisTurn(instanceID) {
		e.recordSelectedInstanceIDWithSource(instanceID, "", SelectedInstanceSourceUser)
	}

	// INV-11: one in-instance run per turn. Check-then-set BEFORE confirm — a user
	// who declines the card still spends the turn's single slot, so the agent cannot
	// re-prompt with a one-word Task tweak (which would also dodge the DB dedup key).
	// A rejected target does NOT spend this slot: no instance was entered and no
	// authorization card was shown.
	if e.instanceOpsRanThisTurn {
		msg := "本回合已经执行过一次实例内排查，不再重复进入实例。如需再次排查，请重新发送指令。"
		onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: msg})
		return friendlyToolResultJSON(msg)
	}
	e.instanceOpsRanThisTurn = true

	// INV-7: the confirm wrapper is conditional (engine.go:1109), so e.confirmFn can
	// be nil on a path that never installed one. Fail closed — never fetch a
	// credential or spawn the harness without an authorization gate.
	if e.confirmFn == nil {
		msg := "当前会话无法进行授权确认，已跳过实例内排查。"
		onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: msg})
		return friendlyToolResultJSON(msg)
	}

	// A decline is non-terminal so the Agent can still answer from cloud-side facts.
	confirmArgs := e.safeExecutor.RedactArgs(action, filtered)
	confirmArgs["Task"] = task
	if !e.confirmFn(action, confirmArgs) {
		msg := instanceOpsUnauthorizedMessage(instanceID, e.lastConfirmationTerminalReason)
		onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: msg})
		return friendlyToolResultJSON(msg)
	}
	// The approved entry card also establishes a genuine selection when the target
	// came from a non-literal binding. A literal user designation is already stored
	// at turn entry; an observed or model-elected target reaches this point only
	// after the card. Record before Run because a failed SSH attempt does not undo
	// what the user chose to diagnose.
	e.recordSelectedInstanceIDWithSource(instanceID, "", SelectedInstanceSourceUser)

	// Connected and command progress become bounded activity events. Command
	// output never enters this stream; only metadata does.
	commandsEmitted := 0
	// Settled commands are accumulated separately from the emitted ones. The live stream is capped
	// at maxInstanceOpsStepEvents because it is a UI feed, but the interruption notice has to be
	// able to report all settled commands over the WHOLE run — a cap on the feed must not
	// silently become a cap on what the user is told a killed run did.
	var settled []instanceOpsSettledStep
	onProgress := func(p InstanceOpsProgress) {
		switch p.Kind {
		case InstanceOpsProgressBackgroundJob:
			// Side-band session continuity only: not a command, UI step, trace event or audit row.
			e.observeInstanceOpsBackgroundJob(instanceID, p.JobID, p.JobState, p.JobPurpose)
		case InstanceOpsProgressAgentSession:
			e.observeInstanceOpsAgentSession(instanceID, p.AgentSessionID,
				p.AgentSessionContract, p.AgentSessionModel)
		case InstanceOpsProgressConnected:
			onStep(StepEvent{
				Type:    StepToolCall,
				Action:  action,
				Source:  observability.ToolSourceDiagnosisInternal,
				Message: fmt.Sprintf("已连接到实例 %s，开始%s", instanceID, instanceOpsPhaseNoun()),
			})
		case InstanceOpsProgressCommand:
			e.observeInstanceOpsBackgroundJob(instanceID, p.JobID, p.JobState, p.JobPurpose)
			settled = append(settled, instanceOpsSettledStep{
				Command:     p.Command,
				Tier:        p.Tier,
				Disposition: p.Disposition,
				Reason:      p.Reason,
			})
			if commandsEmitted >= maxInstanceOpsStepEvents {
				return // capped; the summary still reports the true totals
			}
			commandsEmitted++
			onStep(instanceOpsCommandStep(action, p))
		}
	}

	modelContext.PendingBackgroundJob = e.backgroundJobForInstance(instanceID)
	modelContext.BackgroundJobSlotBusy = e.backgroundJobSlotBusyForOtherInstance(instanceID)
	modelContext.AgentSession = e.instanceOpsAgentSessionForRun(instanceID)
	verdict, err := e.instanceOps.Run(ctx, InstanceOpsRequest{
		TurnID:                e.currentTurnID,
		InstanceID:            instanceID,
		Task:                  task,
		Context:               modelContext,
		RepairScopeAuthorized: true,
	}, onProgress)
	if err != nil {
		// A control-plane NotFound result is authoritative for this target: its guest job can no
		// longer be polled and must not occupy the conversation's only observable-job slot forever.
		// Clear before composing the interruption so no notice falsely promises a retained cursor.
		if errors.Is(err, ErrInstanceOpsNotFound) {
			e.clearBackgroundJobForInstance(instanceID)
		}
		// The run ended with no verdict. Whatever settled before it did is the only account the user
		// will ever get of a box that may already have been changed — so stash it for the next turn.
		// A run with neither a settled command nor a background-job handle stays silent, which keeps
		// every preflight branch below (no SSH target, not found, not running, address unavailable)
		// from implying the box was entered.
		//
		// A pre-launch background-job handle also counts even when no command result made it back: the
		// detached process may have survived the killed harness. Stashed BEFORE the branches rather
		// than inside each, so a future branch cannot return without considering it.
		e.recordInstanceOpsInterruption(instanceID, settled)
		// No SSH entrypoint (empty SshLoginCommand — e.g. a Windows instance): the box
		// can never be entered, so this is honest and NON-retryable. Never the generic
		// "请稍后重试" text, which wrongly implies a transient failure the user should
		// retry. Confirmed live against a CompShare Windows GPU instance.
		if errors.Is(err, ErrInstanceOpsNoSSHTarget) {
			msg := "该实例没有 SSH 登录入口（如 Windows 实例），暂不支持实例内排查。"
			onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: msg})
			return finalReplyPrefix + msg
		}
		// A well-formed account response that omits the id is permanent for this
		// request; tell the user to correct the target rather than retry blindly.
		if errors.Is(err, ErrInstanceOpsNotFound) {
			msg := fmt.Sprintf("在当前账号下找不到实例 %s，可能已被删除 / 释放，或实例 ID 有误。请到控制台核对实例 ID 后再试。", instanceID)
			onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: msg})
			return finalReplyPrefix + msg
		}
		// Address derivation failed before the lane entered the instance. Report only
		// that observable boundary: it does not identify the underlying cause or prove
		// whether the instance itself is healthy.
		if errors.Is(err, ErrInstanceOpsAddressUnavailable) {
			msg := "无法换算该实例的内网地址，本次没有进入实例，也没有执行任何实例内命令。当前只能确认诊断入口未建立，尚无法判断根因，也不能据此判断实例本身是否异常。请稍后重试；如需立即验证，可按控制台显示的登录地址、端口和用户名尝试登录。"
			onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: msg})
			return finalReplyPrefix + msg
		}
		// Candidate addresses were available, but the TCP prerequisite for SSH did
		// not connect. This is still pre-entry: no authentication and no guest command.
		// List the possible layers without selecting one that was never observed.
		if errors.Is(err, ErrInstanceOpsSSHPreflightUnreachable) {
			msg := "诊断服务已尝试可用候选地址，但未能与实例的 SSH 端口建立 TCP 连接；未建立 SSH 会话、未进入实例，也没有执行任何命令。可能涉及诊断服务到实例的网络路径、端口 / 防火墙、SSH 服务或实例当时状态；仅凭本次失败无法确定具体原因，也无法判断用户原始故障是否属于实例内部。"
			onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: msg})
			return finalReplyPrefix + msg
		}
		// The state is a current platform fact, so return it instead of a generic retry.
		if errors.Is(err, ErrInstanceOpsNotRunning) {
			msg := fmt.Sprintf("该实例当前状态为 %s，不是运行中（Running），无法进入实例排查。等实例恢复运行后可以再试。",
				instanceOpsStateFromError(err))
			onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: msg})
			return finalReplyPrefix + msg
		}
		// Honest terminal failure — never let the model narrate a root cause the
		// harness did not reach. The reason class is a constant; the underlying
		// error (already credential-free) is not surfaced to the user verbatim.
		msg := "实例内排查未能完成，请稍后重试，或到控制台查看实例状态。"
		onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: msg})
		return finalReplyPrefix + msg
	}

	// The agentic loop has no known total, so report a count without a fake N/M denominator.
	onStep(StepEvent{
		Type:    StepToolResult,
		Action:  action,
		Source:  observability.ToolSourceDiagnosisInternal,
		Message: fmt.Sprintf("排查完成，共执行 %d 条命令（拒绝 %d 条），正在生成结论", verdict.Ran, verdict.Refused),
	})

	// The verdict is a deterministic final reply: finalReplyPrefix routes it straight
	// out through agentruntime.Final, structurally beyond any synthesis rewrite (F6).
	return finalReplyPrefix + verdict.Text
}

// instanceOpsUnauthorizedMessage reports the actual confirmation outcome. Every
// branch is explicit that no command ran inside the instance.
func instanceOpsUnauthorizedMessage(instanceID, terminalReason string) string {
	switch strings.TrimSpace(terminalReason) {
	case observability.ConfirmationReasonTimeout:
		return fmt.Sprintf(
			"授权卡片已超时（等待 %d 秒未收到确认），未进入实例 %s，也没有执行任何命令。需要的话请重新发送指令。",
			int(InstanceOpsConfirmWindow.Seconds()), instanceID)
	case observability.ConfirmationReasonClientDisconnect:
		return fmt.Sprintf("连接已断开，授权未完成，未进入实例 %s，也没有执行任何命令。重新连接后可以再发一次指令。", instanceID)
	case observability.ConfirmationReasonDeliveryFailed, observability.ConfirmationReasonBrokerCancelled:
		return fmt.Sprintf("授权卡片未能送达，未进入实例 %s，也没有执行任何命令。请重新发送指令。", instanceID)
	default:
		// ConfirmationReasonUserDeclined, and the normalizer's fallback for a
		// legacy boolean callback that cannot say why.
		return fmt.Sprintf("好的，已取消对实例 %s 的实例内排查。", instanceID)
	}
}

// InstanceOpsConfirmWindow is how long the transport gives a person to answer an
// authorization card, quoted in the timeout message so the user learns the
// actual window rather than a number to guess at.
//
// It duplicates httpapi.confirmWaitTimeout by VALUE and not by import, because
// engine must not depend on a transport package — and the duplicate is pinned
// by TestInstanceOpsConfirmWindowMatchesTheTransport in the httpapi package,
// which can see both. A drift here only mis-states a number in one sentence; it
// cannot change when the card actually expires.
const InstanceOpsConfirmWindow = 120 * time.Second

// instanceOpsTargetMayReachConfirmation decides whether an instance id may be
// shown on the SSH entry card. A current deterministic binding reaches that card
// normally. An expired, genuinely user-selected historical id may also reach a
// NEW card for that same id; the card is the fresh selection proof, not the old
// selection. An observed list row can never take either path.
//
// The runner still point-checks that the id belongs to this account after the
// card; neither branch here grants instance entry by itself.
func (e *Engine) instanceOpsTargetMayReachConfirmation(instanceID string) bool {
	if e == nil || !e.turnContextViewReady || strings.TrimSpace(instanceID) == "" {
		return false
	}
	view := e.turnContextViewThisTurn
	binding := e.bindInstanceTarget(view, instanceID)
	if binding.conflict {
		return false
	}
	if binding.bound() {
		return strings.EqualFold(strings.TrimSpace(binding.id), strings.TrimSpace(instanceID))
	}
	// The immutable view is intentionally compiled before the ReAct loop. A
	// resource read may have refreshed the registry since then; a complete
	// one-instance account is a current, unambiguous proof and is not a list-row
	// guess. Never use it to override an explicit unresolved user reference.
	if !binding.explicit {
		if id, _ := e.singleRegistryInstance(); id != "" {
			return strings.EqualFold(strings.TrimSpace(id), strings.TrimSpace(instanceID))
		}
		if expiredUserSelectionMatches(view, instanceID) {
			return true
		}
	}
	// A cold rehydrated session may not have a complete registry yet. An exact ID
	// visibly authored by the user is still a choice, unlike an ID invented from a
	// prior list; account membership is verified by the runner's point describe.
	return e.userNamedInstanceThisTurn(instanceID) || e.userScreenshotNamesInstanceThisTurn(instanceID)
}

// userNamedInstanceThisTurn verifies the model-supplied ID against the user's
// own current message. Both the target gate and designation recorder use it.
func (e *Engine) userNamedInstanceThisTurn(instanceID string) bool {
	if e == nil || !e.turnContextViewReady || strings.TrimSpace(instanceID) == "" {
		return false
	}
	return entity.TextExplicitlyMentionsName(e.turnContextViewThisTurn.CurrentQuestion, instanceID)
}

// userScreenshotNamesInstanceThisTurn accepts one exact instance ID from the
// current screenshot only as a candidate for the existing authorization card.
// OCR remains outside CurrentQuestion, routing and write provenance; the runner
// still verifies account ownership and state after the user confirms the card.
func (e *Engine) userScreenshotNamesInstanceThisTurn(instanceID string) bool {
	if e == nil || strings.TrimSpace(instanceID) == "" {
		return false
	}
	tokens := e.registry.Snapshot().InstanceIDTokensInText(e.imageContextThisTurn)
	if len(tokens) != 1 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(tokens[0]), strings.TrimSpace(instanceID))
}

// expiredUserSelectionMatches is deliberately narrower than a carried selection:
// freshness=expired excludes it from bindInstanceTarget, so it cannot silently
// execute. This helper only lets the old, exact user choice be placed on a new
// card. Keeping the source test here prevents an account-list observation from
// acquiring the same privilege after its TTL elapses.
func expiredUserSelectionMatches(view AgentContext, instanceID string) bool {
	for _, ent := range view.SelectedEntities {
		if ent.Kind != "instance" || ent.Source != SelectedInstanceSourceUser ||
			ent.Freshness != ContinuityFreshnessExpired {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(ent.ID), strings.TrimSpace(instanceID)) {
			return true
		}
	}
	return false
}

// instanceOpsCommandStep maps one command-progress event to its StepEvent. The
// "failed" disposition rides StepBlocked, NOT StepError: both trace recorders
// retain only a closed-set error class, while the UI still receives the
// user-facing message. StepBlocked additionally states that the operation did
// not execute.
func instanceOpsCommandStep(action string, p InstanceOpsProgress) StepEvent {
	cmd := truncateCommandForStep(p.Command)
	var msg string
	stepType := StepToolResult
	switch p.Disposition {
	case "ran":
		if p.ExitCode != nil {
			msg = fmt.Sprintf("`%s` → 已执行（exit %d，%d B）", cmd, *p.ExitCode, p.Bytes)
		} else {
			msg = fmt.Sprintf("`%s` → 已执行（%d B）", cmd, p.Bytes)
		}
	case "refused":
		stepType = StepBlocked
		msg = fmt.Sprintf("`%s` → 已拒绝：%s", cmd, instanceOpsRefusalReason(p.Reason))
	default: // "failed" and any unexpected value → honest failure on the constant sink
		stepType = StepBlocked
		msg = fmt.Sprintf("`%s` → 执行失败", cmd)
	}
	return StepEvent{
		Type: stepType, Action: action, Source: observability.ToolSourceDiagnosisInternal,
		Message: msg, ErrorCode: instanceOpsTraceErrorCode(p),
	}
}

// instanceOpsTraceErrorCode mechanically exposes the harness's existing closed
// disposition/reason signals. Command text and user-facing explanations are
// never inspected, and there is no second reason-to-code lookup table to drift.
func instanceOpsTraceErrorCode(p InstanceOpsProgress) string {
	switch p.Disposition {
	case "ran":
		return ""
	case "failed":
		return "SSH_COMMAND_FAILED"
	case "refused":
		reason := strings.TrimSpace(p.Reason)
		if reason == "" {
			return "SSH_REFUSED"
		}
		if _, known := knownInstanceOpsRefusalReason(reason); !known {
			return tools.TraceAgentToolErrorOther
		}
		return tools.TraceAgentToolErrorCode("SSH_" + strings.ToUpper(reason))
	default:
		return tools.TraceAgentToolErrorOther
	}
}

// truncateCommandForStep bounds a command to 200 runes before it enters a
// user-visible / persisted Message. Rune-based so a multibyte path never splits.
func truncateCommandForStep(s string) string {
	const maxCommandRunes = 200
	r := []rune(s)
	if len(r) <= maxCommandRunes {
		return s
	}
	return string(r[:maxCommandRunes]) + "…"
}

// instanceOpsStateFromError recovers the instance state the runner reported, from
// the "<sentinel>: <state>" wrapping the cmd adapter applies. The engine cannot
// import internal/sshops (that dependency is exactly what the sentinel exists to
// avoid), so this is the seam where the string is read back — one place, and it
// degrades to a neutral word rather than printing a malformed message.
func instanceOpsStateFromError(err error) string {
	if err == nil {
		return "未知"
	}
	if _, state, found := strings.Cut(err.Error(), ErrInstanceOpsNotRunning.Error()+": "); found {
		if state = strings.TrimSpace(state); state != "" {
			return state
		}
	}
	return "未知"
}

// instanceOpsPhaseNoun and instanceOpsRefusalReason keep the live activity stream truthful about
// which product is running. Both read the same boot flag the tool description does, so the card the
// user approved, the tool the model was offered, and the line it watches scroll all say one thing.
func instanceOpsPhaseNoun() string {
	return "排查"
}

// instanceOpsRefusalReason maps the harness's closed reasons to actionable user
// text. Unknown reasons keep a conservative compatibility fallback.
func instanceOpsRefusalReason(reason string) string {
	if message, ok := knownInstanceOpsRefusalReason(reason); ok {
		return message
	}
	return "属于高危操作或命令形式不被接受"
}

func knownInstanceOpsRefusalReason(reason string) (string, bool) {
	switch reason {
	case "refused_destructive":
		return "属于高危操作，已硬性拒绝", true
	case "refused_form":
		return "命令形式不被接受（含命令替换）；请移除命令替换后重发", true
	case "refused_user_declined":
		return "你未批准这条命令，命令未执行", true
	case "refused_confirmation_timeout":
		return fmt.Sprintf("等待你的确认超过 %d 秒，命令未执行", int(InstanceOpsConfirmWindow.Seconds())), true
	case "refused_client_disconnect":
		return "连接已断开，命令未执行", true
	case "refused_confirmation_delivery_failed":
		return "确认卡片未能送达，命令未执行", true
	case "refused_confirmation_broker_cancelled":
		return "确认请求已取消，命令未执行", true
	case "refused_not_approved":
		// A pre-terminal-reason harness can only prove that no approval arrived.
		// Do not turn that absence into a claim that the user explicitly declined.
		return "未收到对这条命令的确认，命令未执行", true
	case "refused_unconfirmable":
		return "命令超过实例内运维工具输入上限，未执行", true
	case "refused_precondition":
		return "前置条件未满足，操作未执行；请按工具返回的具体原因检查参数或重新读取目标状态后重试", true
	case "refused_no_progress":
		return "相同只读检查的结果没有变化，已停止重复；请更换检查条件、等待真实状态变化或结束本轮排查", true
	case "refused_mutating_phase1":
		return "旧版只读执行器未运行这条修改命令，请重新发起实例内排查", true
	}
	return "", false
}
