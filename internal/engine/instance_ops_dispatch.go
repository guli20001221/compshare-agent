package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/tools"
)

// maxInstanceOpsStepEvents caps the per-command activity events the engine emits
// for one in-instance diagnosis. It mirrors the harness's own step cap
// (maxHarnessSteps=50) and the durable per-step synchronous INSERT it drives, so a
// runaway harness cannot flood the activity stream or the turn's DB writes. Beyond
// the cap, per-command events stop; the terminal summary still reports the totals.
const maxInstanceOpsStepEvents = 50

// instanceOpsWriteAction names the per-command approval on the wire. It is deliberately NOT
// DiagnoseInstanceInternals: that card authorizes entering the box, this one authorizes one
// specific change, and a user who sees the same label twice cannot tell which they answered.
const instanceOpsWriteAction = "InstanceOpsWriteCommand"

// executeInstanceOps handles a DiagnoseInstanceInternals tool call: the read-only
// in-instance diagnosis lane. It is dispatched from executeToolOnce BEFORE the
// diagnosis-chain and mutating-tool branches, so it never inherits the
// SafeToolExecutor per-attempt wall-clock ceiling. The whole lane is inert unless
// a runner was wired (server SharedDeps.InstanceOps / CLI SetInstanceOps) — when
// e.instanceOps is nil the tool is not even in the model's window (see
// centralAgentToolWindow), and this method fails closed on the off chance the model
// names it anyway.
//
// Ordering (plan §3.1 / the security invariants):
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
	// The SSH lane can read and, when separately enabled, modify a box. A non-empty
	// model-authored id is therefore not enough: a preceding account-wide list is
	// observation, not a user selection. Reuse the exact selection proof used by
	// workflow writes, so "刚才那台" keeps working after a genuine pick while a
	// vague symptom cannot silently target the first running list row.
	if !e.instanceOpsTargetIsSelected(instanceID) {
		msg := "请先明确要排查的实例。多个实例时请让用户提供实例 ID/名称或从候选列表选择；不能自行从实例列表挑选。"
		onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: msg})
		return friendlyToolResultJSON(msg)
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

	// Authorization card. A decline is honest and non-terminal: the turn continues
	// so the agent can still answer from cloud-side facts. The returned text is NOT
	// a finalReplyPrefix (plan P1 门 3) and is distinct from the "already ran" text.
	if !e.confirmFn(action, e.safeExecutor.RedactArgs(action, filtered)) {
		msg := instanceOpsUnauthorizedMessage(instanceID, e.lastConfirmationTerminalReason)
		onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: msg})
		return friendlyToolResultJSON(msg)
	}

	// Live activity stream. connected + each command become one StepEvent; the
	// terminal summary is emitted below from the verdict tallies. Per-command
	// events are capped at maxInstanceOpsStepEvents (defense in depth with the
	// harness's own cap). Command output NEVER appears here (INV-6) — only metadata.
	commandsEmitted := 0
	onProgress := func(p InstanceOpsProgress) {
		switch p.Kind {
		case InstanceOpsProgressConnected:
			onStep(StepEvent{
				Type:    StepToolCall,
				Action:  action,
				Source:  observability.ToolSourceDiagnosisInternal,
				Message: fmt.Sprintf("已连接到实例 %s，开始%s", instanceID, instanceOpsPhaseNoun()),
			})
		case InstanceOpsProgressCommand:
			if commandsEmitted >= maxInstanceOpsStepEvents {
				return // capped; the summary still reports the true totals
			}
			commandsEmitted++
			onStep(instanceOpsCommandStep(action, p))
		}
	}

	// Per-command approval for writes. Reads are not gated: 20-45 of the commands in a real run are
	// reads, and a card for each would train the user to click through without reading — which is
	// worse than no card. Only the 1-3 that change the box stop and ask, and the card shows the
	// literal command, because "may I change something" is not a question anyone can answer.
	var confirmWrite func(string) bool
	if tools.InstanceOpsWritesEnabled() {
		confirmWrite = func(command string) bool {
			return e.confirmFn(instanceOpsWriteAction, map[string]any{
				"UHostId": instanceID,
				"Command": command,
			})
		}
	}

	verdict, err := e.instanceOps.Run(ctx, InstanceOpsRequest{
		TurnID:       e.currentTurnID,
		InstanceID:   instanceID,
		Task:         task,
		ConfirmWrite: confirmWrite,
	}, onProgress)
	if err != nil {
		// No SSH entrypoint (empty SshLoginCommand — e.g. a Windows instance): the box
		// can never be entered, so this is honest and NON-retryable. Never the generic
		// "请稍后重试" text, which wrongly implies a transient failure the user should
		// retry. Confirmed live against a CompShare Windows GPU instance.
		if errors.Is(err, ErrInstanceOpsNoSSHTarget) {
			msg := "该实例没有 SSH 登录入口（如 Windows 实例），暂不支持实例内排查。"
			onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: msg})
			return finalReplyPrefix + msg
		}
		// Not found: the id is not in this account. Retrying it can never succeed, so the
		// generic 「请稍后重试」 is advice that cannot work — and this is the most common way
		// the lane fails in practice, because instance ids go stale fast (a test account
		// replaced 7 of 10 instances within one hour on 2026-08-06, and a diagnosis aimed at
		// an id read minutes earlier died here while the message pointed at a transient
		// problem). A transient describe failure keeps the retry text: only a well-formed
		// response that did not contain this id lands here.
		if errors.Is(err, ErrInstanceOpsNotFound) {
			msg := fmt.Sprintf("在当前账号下找不到实例 %s，可能已被删除 / 释放，或实例 ID 有误。请到控制台核对实例 ID 后再试。", instanceID)
			onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: msg})
			return finalReplyPrefix + msg
		}
		// The internal address could not be derived, so the lane refused rather than dialling
		// the public address it is configured not to use. This is a DEPLOYMENT-side failure —
		// the internal gateway, its config, or the region lookup — and the user's instance is
		// not implicated, so the generic 「请稍后重试，或到控制台查看实例状态」 would send them
		// to look at a console that has nothing wrong on it.
		//
		// It gets its own sentence for a second reason: the internal-IPv6 route ships without
		// ever having run against the real backend (nothing routes to the internal gateway from
		// a development machine), so the first production run IS the experiment. Three outcomes
		// have to be told apart from the reply alone, by someone who may not be able to read the
		// server log: it worked, the address could not be derived (here), or the address was
		// derived and the dial to it failed (#522's dial text, which now also names the route).
		if errors.Is(err, ErrInstanceOpsAddressUnavailable) {
			msg := "无法换算该实例的内网地址，本次没有进入实例。这是运行环境侧的问题（内网网关或其配置），与实例本身无关，请联系部署同学查看服务日志。"
			onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: msg})
			return finalReplyPrefix + msg
		}
		// Not running: name the state. This used to fall into the generic branch below
		// and tell the user 「请稍后重试，或到控制台查看实例状态」 — advice to go look up
		// the one fact we already had in hand. The state comes verbatim from the same
		// describe response; nothing here translates or guesses it.
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

	// Terminal summary. No N/M denominator — the harness's agentic loop has no known
	// total (see plan §5.2 and commit 889ad277); a bare count avoids a fake progress bar.
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

// instanceOpsUnauthorizedMessage phrases a card that was not approved, using the
// reason the confirmation harness already computed.
//
// One sentence used to cover every non-approval, and it named the user as the
// actor: 「好的，已取消对实例 X 的实例内排查」. Production traces for 2026-07-13..08-12
// show that was wrong far more often than it was right — of 104 entry cards, 9
// ended in a timeout against 2 genuine declines, so the most common reader of
// that sentence was someone who had NOT cancelled anything and, in the console,
// had just clicked 确认 a moment too late and received a bare [NotFound] beside
// it. A reply that misattributes the cause also misdirects the retry: "已取消"
// invites re-asking, while a timeout wants "answer the card faster" and a
// disconnect wants "reconnect".
//
// Deliberately no reason is phrased as a failure the user must report: every
// branch here is a turn where NOTHING was executed inside the instance, and
// saying so plainly is the point of the message.
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

// instanceOpsTargetIsSelected returns true only when the instance passed to the
// SSH-ops lane has a selection proof from this turn. It deliberately shares the
// workflow binder rather than treating a freshly observed account-list row as a
// referent. A literal id the user typed is allowed on a cold session too: the
// runner will still re-check that it belongs to the current account before entry.
func (e *Engine) instanceOpsTargetIsSelected(instanceID string) bool {
	if e == nil || !e.turnContextViewReady || strings.TrimSpace(instanceID) == "" {
		return false
	}
	view := e.turnContextViewThisTurn
	binding := e.bindInstanceTarget(view)
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
	}
	// A cold rehydrated session may not have a complete registry yet. An exact ID
	// visibly authored by the user is still a choice, unlike an ID invented from a
	// prior list; account membership is verified by the runner's point describe.
	return entity.TextExplicitlyMentionsName(view.CurrentQuestion, instanceID)
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
	return StepEvent{Type: stepType, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: msg}
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
	if tools.InstanceOpsWritesEnabled() {
		return "排查"
	}
	return "只读排查"
}

// A refusal in write mode is NOT "只读模式" — that wording sent the operator looking for a switch
// that was already on. In write mode the only refusals left are the destructive tier and the shape
// gate, so the reason has to name those instead.
//
// And "those" is plural, which is the whole problem the `reason` argument fixes. The harness writes
// six distinct dispositions and the wire carried three, so this function had nothing to read and
// answered every write-mode refusal with one sentence covering the destructive tier, the shape gate,
// an over-long command and a card the operator declined. 「属于高危操作或命令形式不被接受」 is not a
// fact anyone can act on: the operator cannot tell a policy refusal from their own click, and the
// model cannot tell "never going to work" from "resend it split in two" — measured in #516's class,
// where an unactionable refusal made the run delete half its own probe chain and retry.
//
// An unknown or empty reason keeps exactly the previous wording, so a server ahead of the harness
// (or behind it) degrades instead of printing a blank.
func instanceOpsRefusalReason(reason string) string {
	switch reason {
	case "refused_destructive":
		return "属于高危操作，已硬性拒绝"
	case "refused_form":
		return "命令形式不被接受（含命令替换或多行），需拆成单条命令重发"
	case "refused_not_approved":
		return "你未批准这条命令"
	case "refused_unconfirmable":
		return "命令过长，无法完整展示在确认卡上"
	case "refused_unmanaged_platform_service":
		return "未核实平台入口契约，不能直接启动 FileBrowser；需先确认镜像服务管理方式和平台映射"
	case "refused_mutating_phase1":
		return "会修改实例环境（只读模式）"
	}
	if tools.InstanceOpsWritesEnabled() {
		return "属于高危操作或命令形式不被接受"
	}
	return "会修改实例环境（只读模式）"
}
