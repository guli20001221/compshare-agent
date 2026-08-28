package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/compshare-agent/internal/entity"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/opscontext"
	"github.com/compshare-agent/internal/security"
	"github.com/compshare-agent/internal/tools"
)

// maxInstanceOpsStepEvents caps the per-command activity events the engine emits
// for one in-instance diagnosis. It mirrors the harness's own step cap
// (maxHarnessSteps=50) and the per-step audit write it drives, so a
// runaway harness cannot flood the activity stream or the turn's DB writes. Beyond
// the cap, per-command events stop; the terminal summary still reports the totals.
const maxInstanceOpsStepEvents = 50

// instanceOpsEntryBoundaryData is the narrow observation emitted when this lane
// cannot cross a prerequisite Guest-entry boundary. It deliberately names the
// observer and the boundary rather than turning one access limitation into a
// conclusion about the instance or evidence supplied through another path.
//
// Unlike a terminal reply, this is a normal AgentToolResult. The central Agent
// therefore receives both this observation and the canonical conversation and
// can reconcile independent evidence in one semantic path.
type instanceOpsEntryBoundaryData struct {
	ObservationSource     string `json:"observation_source"`
	ObservationScope      string `json:"observation_scope"`
	ExecutionBoundary     string `json:"execution_boundary"`
	SSHEntrypoint         string `json:"ssh_entrypoint"`
	TCPConnection         string `json:"tcp_connection"`
	SSHSessionEstablished bool   `json:"ssh_session_established"`
	GuestCommandsExecuted bool   `json:"guest_commands_executed"`
	EvidenceBoundary      string `json:"evidence_boundary"`
}

func instanceOpsPreflightFailureObservation(action string) string {
	return tools.MarshalAgentToolResult(tools.AgentToolFailure(action,
		instanceOpsEntryBoundaryData{
			ObservationSource:     "diagnostic_service",
			ObservationScope:      "diagnostic_service_to_instance_ssh_candidate",
			ExecutionBoundary:     "pre_ssh_tcp",
			SSHEntrypoint:         "candidate_available",
			TCPConnection:         "not_established",
			SSHSessionEstablished: false,
			GuestCommandsExecuted: false,
			EvidenceBoundary: "This observation is limited to the diagnostic service path. " +
				"It does not confirm or contradict connectivity or SSH-handshake evidence observed from another vantage point.",
		},
		"SSH_DIAGNOSTIC_VANTAGE_UNREACHABLE",
		"诊断服务未能与候选 SSH 地址建立 TCP 连接；该结果仅描述诊断服务的网络视角，不能据此否定用户从其他位置观察到的连通性或 SSH 握手证据。",
		tools.AgentToolMeta{SourceStatus: "preflight_failed"}))
}

// instanceOpsNoSSHTargetObservation keeps a missing SSH entrypoint as one
// bounded observation instead of ending the central Agent turn. The SSH lane
// cannot enter the Guest, but platform reads and knowledge retrieval observe
// different layers and can still answer or narrow the user's request.
func instanceOpsNoSSHTargetObservation(action string) string {
	return tools.MarshalAgentToolResult(tools.AgentToolFailure(action,
		instanceOpsEntryBoundaryData{
			ObservationSource:     "platform_instance_access",
			ObservationScope:      "instance_guest_ssh_entry",
			ExecutionBoundary:     "no_ssh_entrypoint",
			SSHEntrypoint:         "not_available",
			TCPConnection:         "not_attempted",
			SSHSessionEstablished: false,
			GuestCommandsExecuted: false,
			EvidenceBoundary: "This observation only proves that the instance has no SSH entrypoint available to this diagnostic lane. " +
				"It does not prevent the central agent from using platform read capabilities or knowledge evidence.",
		},
		"INSTANCE_GUEST_SSH_UNAVAILABLE",
		"该实例没有可用的 SSH 登录入口；未建立 SSH 会话、未进入 Guest，也没有执行 Guest 命令。仍可继续查询平台实时事实和知识证据。",
		tools.AgentToolMeta{SourceStatus: "no_ssh_target"}))
}

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
// deployment-authorized in-instance diagnosis and repair lane. It is dispatched from executeToolOnce BEFORE the
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
//   - write grant → the deployment must have enabled autonomous mutating tools
//   - target proof → the user named/selected this exact instance (never pick a list row)
//   - INV-11 gate → at most one in-instance run per turn
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
	// SSH-ops is one autonomous diagnose/repair task, not a read-only probe with a
	// hidden write mode. The server-owned deployment setting is the standing grant;
	// a model-authored tool call cannot turn the lane on when it is disabled.
	if !e.mutatingToolsEnabled {
		msg := "实例内排查与修复未在当前环境启用。"
		onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: msg})
		return friendlyToolResultJSON(msg)
	}

	filtered := e.safeExecutor.FilterArgs(action, args)
	instanceIDArg, _ := filtered["UHostId"].(string)
	taskArg, _ := filtered["Task"].(string)
	modeArg, _ := filtered["Mode"].(string)
	instanceID := strings.TrimSpace(instanceIDArg)
	task := strings.TrimSpace(taskArg)
	if instanceID == "" || task == "" {
		msg := "请提供要排查的实例 ID 和本次排查目标。"
		onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: msg})
		return friendlyToolResultJSON(msg)
	}
	// Mode is a capability boundary, not merely prompt wording. Explicit inspection reaches the
	// same SSH/read surface while the private repair bit stays false, so every mutation is refused
	// by the harness at runtime. Missing and unknown values fail closed instead of silently granting
	// repair authority to a malformed/replayed model call.
	repairScopeAuthorized := false
	switch strings.ToLower(strings.TrimSpace(modeArg)) {
	case "repair":
		repairScopeAuthorized = true
	case "inspect":
	default:
		msg := "实例内任务模式无效；只支持 inspect（只读）或 repair（可恢复修复）。"
		onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: msg})
		return friendlyToolResultJSON(msg)
	}
	// Build the local-only portion before any platform call. A planner that copied
	// a user Authorization value into Task must not move it into audit or activity;
	// private values stay json:"-" and are handed only to the runner.
	modelContext := e.instanceOpsModelContext()
	authorizations := make([]string, 0, len(modelContext.ProbeAuthorizations))
	for _, item := range modelContext.ProbeAuthorizations {
		authorizations = append(authorizations, item.Value)
	}
	task = security.RedactKnownAuthorizationText(task, authorizations)
	// A long pause may outlive the pooled Engine that originally knew the
	// account's instance names. Refresh only when this lane is actually invoked,
	// before target binding, so a current exact name can switch A to B without an
	// account-wide read on every unrelated chat turn. Failure remains best-effort:
	// exact current IDs still go through the runner's tenant-scoped point lookup.
	e.refreshInstanceRegistryForStickySelection(ctx)
	// A model-authored id is not selection proof. The proof is either an explicit
	// current-turn target or the same conversation's last genuine user_selected
	// target. The latter is intentionally not revoked by elapsed wall-clock time:
	// this lane has no entry card, and real users routinely resume the same repair
	// after a long-running download, training job, or an overnight pause.
	if !e.instanceOpsTargetAuthorized(instanceID) {
		onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: instanceOpsTargetRefusalForUser})
		return friendlyToolResultJSON(instanceOpsTargetRefusalForModel)
	}
	// A cold registry can make a literal ID provable only at this gate. Record the
	// user's designation before entry. Account-single, screenshot-only and
	// passively observed targets do not authorize this path and are never recorded here.
	if e.userNamedInstanceThisTurn(instanceID) || e.userResolvedInstanceThisTurn(instanceID) {
		e.recordSelectedInstanceIDWithSource(instanceID, "", SelectedInstanceSourceUser)
	}

	// INV-11: one in-instance run per turn. A rejected target does not spend this
	// slot because the instance was never entered.
	if e.instanceOpsRanThisTurn {
		msg := "本回合已经执行过一次实例内排查，不再重复进入实例。如需再次排查，请重新发送指令。"
		onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: msg})
		return friendlyToolResultJSON(msg)
	}
	e.instanceOpsRanThisTurn = true

	// Deterministic binding, not a confirmation card, proves the selected target.
	// Record before Run because a failed SSH attempt does not undo what the user
	// chose to diagnose.
	e.recordSelectedInstanceIDWithSource(instanceID, "", SelectedInstanceSourceUser)

	// Connected and command progress become bounded activity events. Command
	// output never enters this stream; only metadata does.
	commandsEmitted := 0
	modelContext.BridgeConversationAnchor = opscontext.ConversationAnchor(modelContext.ConversationHistory)
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
			if p.AgentSessionConversationAnchor != modelContext.BridgeConversationAnchor {
				return
			}
			e.observeInstanceOpsAgentSession(instanceID, p.AgentSessionID, p.AgentSessionWorkdirID,
				p.AgentSessionContract, p.AgentSessionModel, p.AgentSessionConversationAnchor)
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
		RepairScopeAuthorized: repairScopeAuthorized,
	}, onProgress)
	if err != nil {
		// A control-plane NotFound result is authoritative for this target: its guest job can no
		// longer be polled and must not occupy the conversation's only observable-job slot forever.
		// Clear before composing the interruption so no notice falsely promises a retained cursor.
		if errors.Is(err, ErrInstanceOpsNotFound) {
			e.clearBackgroundJobForInstance(instanceID)
			e.clearSelectedInstanceIfMatches(instanceID)
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
		// No SSH entrypoint is an authoritative boundary for this lane, but not for the
		// whole central Agent. Return it as a structured observation so platform reads
		// and knowledge retrieval remain available; never imply that Guest commands ran.
		if errors.Is(err, ErrInstanceOpsNoSSHTarget) {
			msg := "该实例没有可用的 SSH 登录入口；未建立 SSH 会话、未进入 Guest，也没有执行 Guest 命令。"
			onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: msg})
			return instanceOpsNoSSHTargetObservation(action)
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
		// This must remain a normal tool observation, rather than a deterministic
		// final reply: the central Agent may already have independent user evidence
		// from another network vantage point. Returning a terminal reply here used
		// to overwrite that evidence before the Agent could reconcile it.
		if errors.Is(err, ErrInstanceOpsSSHPreflightUnreachable) {
			msg := "诊断服务未能与候选 SSH 地址建立 TCP 连接；未建立 SSH 会话、未进入实例，也没有执行任何命令。"
			onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceDiagnosisInternal, Message: msg})
			return instanceOpsPreflightFailureObservation(action)
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

// instanceOpsTargetAuthorized decides whether the target was selected by the
// user strongly enough for the deployment-authorized SSH lane to enter it with
// no extra card. Authority comes from one of two deterministic sources:
//
//   - an explicit current-turn ID, exact name, wrapper, or ordinal resolved
//     against a list the user actually saw;
//   - the latest genuine user_selected instance carried by this conversation.
//
// Account-single fallback, passive reads, model choice, screenshot OCR and an
// unresolved/conflicting current reference are deliberately excluded. Time alone
// does not revoke a user's selection: a newer explicit target replaces it, while
// the runner still point-checks account ownership and instance state before
// fetching credentials or dialing.
func (e *Engine) instanceOpsTargetAuthorized(instanceID string) bool {
	if e == nil || !e.turnContextViewReady || strings.TrimSpace(instanceID) == "" {
		return false
	}
	view := e.turnContextViewThisTurn
	binding := e.bindInstanceTarget(view, instanceID)
	if binding.conflict {
		return false
	}
	if binding.bound() {
		if !strings.EqualFold(strings.TrimSpace(binding.id), strings.TrimSpace(instanceID)) {
			return false
		}
		if binding.explicit {
			return true
		}
		for _, selected := range view.SelectedEntities {
			if selected.Kind == "instance" &&
				selected.Source == SelectedInstanceSourceUser &&
				selected.Freshness != ContinuityFreshnessExpired &&
				strings.EqualFold(strings.TrimSpace(selected.ID), strings.TrimSpace(instanceID)) {
				return true
			}
		}
		return false
	}
	// An explicit but unresolved current reference (a typo, an out-of-range
	// ordinal, or a different cold-registry ID) must never fall back to the old
	// selection. An exact current-turn ID can still authorize through the cold
	// registry path; account membership is verified by the runner's point describe.
	if binding.explicit {
		return e.userNamedInstanceThisTurn(instanceID)
	}
	return false
}

// userNamedInstanceThisTurn verifies the model-supplied ID against the user's
// own current message. Both the target gate and designation recorder use it.
func (e *Engine) userNamedInstanceThisTurn(instanceID string) bool {
	if e == nil || !e.turnContextViewReady || strings.TrimSpace(instanceID) == "" {
		return false
	}
	return entity.TextExplicitlyMentionsName(e.turnContextViewThisTurn.CurrentQuestion, instanceID)
}

// userResolvedInstanceThisTurn verifies a non-literal wrapper (for example an
// access hostname or shell prompt) against the current complete account snapshot.
// Requiring an explicit, uniquely bound current-turn reference keeps passive list
// observations and account-single fallback out of authorization provenance.
func (e *Engine) userResolvedInstanceThisTurn(instanceID string) bool {
	if e == nil || !e.turnContextViewReady || strings.TrimSpace(instanceID) == "" {
		return false
	}
	binding := e.bindInstanceTarget(e.turnContextViewThisTurn, instanceID)
	return binding.explicit && binding.bound() &&
		strings.EqualFold(strings.TrimSpace(binding.id), strings.TrimSpace(instanceID))
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
// which product is running. Both share the same contract as the model-visible tool description,
// so the offered capability and the line the user watches scroll say one thing.
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
		return "等待你的确认已超时，命令未执行", true
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
	case "refused_inspection_scope":
		return "本轮是只读核查，修改操作未执行；我会继续使用只读证据完成核查", true
	}
	return "", false
}
