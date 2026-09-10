package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/compshare-agent/internal/deployment"
	"github.com/compshare-agent/internal/governance"
	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/readprojection"
	"github.com/compshare-agent/internal/security"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
)

// Mutating operations reach execution only as a confirmableAction, and their
// user-facing outcome is composed here rather than narrated by the model. A
// write that may already have committed, a readback mismatch and an
// initialization failure each get their own deterministic sentence, because a
// model that did not observe the write must not describe it.

// confirmableAction is the ONLY input executeResolvedWorkflow accepts. Its fields
// are unexported and its sole constructor (newConfirmableAction) takes a
// resolver-produced resolvedProposal that is already gate-eligible
// (ReadyForConfirmation, or ReadyForIntake for the guided form) — so no caller can
// hand the workflow-execution entry a bare action name + args it invented. The
// guarantee is a compile-time one: bare (action string, args map) can no longer
// reach execution.
//
// The human confirmation gate fires INSIDE executeResolvedWorkflow
// (workflow.Engine.Run → confirmFn), so this carrier is pre-confirmation
// (Confirmable), not yet authorized; the post-confirm seal
// (workflow.SealedActionContract) is a separate, deeper guarantee produced further
// down in the run.
type confirmableAction struct {
	operation string
	args      map[string]any
	refData   workflow.ReferenceData
}

// newConfirmableAction builds the typed execution entry from a resolved proposal.
// It returns ok=false unless the resolver adjudicated the action to a gate-eligible
// state (ReadyForConfirmation or ReadyForIntake), so the only path to
// executeResolvedWorkflow runs through the resolver. The two production callers have
// already established readiness before calling, so they may ignore ok; re-checking
// here makes the invariant a property of the type rather than of each call site.
func newConfirmableAction(rp resolvedProposal) (confirmableAction, bool) {
	if !rp.action.ReadyForConfirmation && !rp.action.ReadyForIntake {
		return confirmableAction{}, false
	}
	return confirmableAction{
		operation: rp.action.Operation,
		args:      rp.action.Arguments,
		refData:   rp.referenceData,
	}, true
}

// executeResolvedWorkflow runs an Agent-proposed action whose parameters and
// exact account target have been verified. The workflow confirms and executes
// those parameters, using the same live catalog snapshot as the resolver.
func (e *Engine) executeResolvedWorkflow(ctx context.Context, act confirmableAction, onStep func(StepEvent)) toolOutcome {
	action, args, refData := act.operation, act.args, act.refData
	e.lastConfirmationAcceptedThisCall = false
	if !e.mutatingToolsEnabled {
		msg := mutatingToolsDisabledMessage
		onStep(blockedStepEvent(action, observability.ToolSourceMainReAct, e.safeExecutor.RedactArgs(action, args), msg, tools.ErrMutatingActionDisabled))
		return deterministicReply(msg)
	}
	// Hard guard (fail-safe) — instance-operation workflows MUST arrive with a
	// non-empty UHostId. A resolved write always carries its dual-proof-verified
	// target, so an empty one here means no target was ever authorized; refuse
	// rather than guess. Account/storage creation workflows do not target an
	// existing instance, so they derive false from workflowRequiresInstanceTarget.
	if workflowRequiresInstanceTarget(action) {
		if uHostId, _ := args["UHostId"].(string); strings.TrimSpace(uHostId) == "" {
			msg := "请先确认要操作的实例。当有多个实例时，请列出实例列表让用户选择后再执行操作。"
			onStep(StepEvent{Type: StepBlocked, Action: action, Source: observability.ToolSourceMainReAct, Message: msg})
			guardResult := map[string]any{"success": false, "message": msg}
			b, _ := json.Marshal(guardResult)
			return observed(string(b))
		}
	}

	wf, ok := workflow.GetWorkflow(action)
	if !ok {
		msg := fmt.Sprintf("未知的工作流: %s", action)
		onStep(StepEvent{Type: StepError, Action: action, Source: observability.ToolSourceMainReAct, Message: msg})
		return observed(msg)
	}
	if e.guidedCreate && e.confirmEditsFn != nil && operationSupportsGuidedIntake(action) {
		wf = workflow.CreateInstanceGuidedDef()
	}

	if decision, ok := e.allowMutatingTool(action); !ok {
		msg := rateLimitMessage(decision.Reason)
		onStep(blockedStepEvent(action, observability.ToolSourceMainReAct, e.safeExecutor.RedactArgs(action, args), msg, governance.ErrRateLimited))
		return deterministicReply(msg)
	}

	var wfConfirm workflow.ConfirmFunc
	if e.confirmFn != nil {
		wfConfirm = workflow.ConfirmFunc(e.confirmFn)
	}

	wfEngine := workflow.NewEngine(e.toolExecutorFor(tools.OriginWorkflowInternal), wfConfirm, func(ev workflow.StepEvent) {
		// A workflow resolve step calls no tool, and this vocabulary has no term
		// for an internal computation: eventType below defaults to StepToolCall
		// and only a workflow.StepToolCall is promoted to StepToolResult on
		// success, so forwarding one would announce a tool call with an empty
		// Action on BOTH its running and success events — a phantom call that
		// never returns, and a trace keyed on an empty action. Its FAILURE does
		// map correctly (StepError, below) and is the only part the user must see.
		if ev.Type == workflow.StepResolve && ev.Status != "failed" {
			return
		}
		eventType := StepToolCall
		message := fmt.Sprintf("[%d/%d] %s: %s", ev.StepIndex+1, ev.Total, ev.StepName, ev.Status)
		if ev.Message != "" {
			message = message + ": " + ev.Message
		}
		capped, capReason := cappedTraceForFriendlyError(nil, ev.Message)
		if ev.Type == workflow.StepConfirm {
			if ev.Status == "waiting" {
				eventType = StepConfirmNeeded
			} else if ev.Status == "cancelled" {
				eventType = StepBlocked
			}
		}
		switch ev.Status {
		case "failed":
			eventType = StepError
			if _, ok := friendlyMessageFromText(ev.Message); ok {
				eventType = StepBlocked
			}
		case "success":
			if ev.Type == workflow.StepToolCall {
				eventType = StepToolResult
			}
		}
		onStep(StepEvent{
			Type:      eventType,
			Action:    ev.Tool,
			Source:    observability.ToolSourceWorkflowInternal,
			Args:      e.safeExecutor.RedactArgs(ev.Tool, ev.Args),
			Message:   message,
			Capped:    capped,
			CapReason: capReason,
		})
	})
	// Opted-in clients may edit declared form fields; every edit is revalidated.
	if e.confirmEditsFn != nil {
		wfEngine.SetConfirmEditsFn(e.confirmEditsFn)
	}

	// GpuType is already canonicalized against the live catalog before confirmation;
	// the card, sealed contract and execution therefore carry the same value.
	if action == "CreateInstanceWorkflow" {
		if gt, _ := args["GpuType"].(string); gt != "" {
			if e.guidedCreate && e.confirmEditsFn != nil {
				if _, preset := args["GuidedGpuLocked"]; !preset {
					args["GuidedGpuLocked"] = true
				}
			}
		}
		if u, ok := tools.UserFrom(ctx); ok {
			if u.TopOrganizationID != 0 {
				args["top_organization_id"] = u.TopOrganizationID
			}
			if u.OrganizationID != 0 {
				args["organization_id"] = u.OrganizationID
			}
		}
	}
	if action == "CreateCFSWorkflow" || action == "ResizeCFSWorkflow" || action == "EnableNetOptimizerWorkflow" {
		if u, ok := tools.UserFrom(ctx); ok {
			if u.TopOrganizationID != 0 {
				args["top_organization_id"] = u.TopOrganizationID
			}
			if u.OrganizationID != 0 {
				args["organization_id"] = u.OrganizationID
			}
		}
	}
	// A user-named availability zone is already resolved against the live catalog;
	// the workflow validates that canonical value against the same snapshot.

	// Share the live proposal catalogs with guided validation. A source change
	// inside the form still reloads the corresponding catalog.
	wfRunOpts := []workflow.RunOption{workflow.WithReferenceData(refData)}

	result, err := wfEngine.Run(ctx, wf, args, wfRunOpts...)
	if err != nil {
		if msg, ok := friendlyToolErrorMessage(err); ok {
			onStep(blockedStepEvent(action, observability.ToolSourceMainReAct, nil, msg, err))
			return deterministicReply(msg)
		}
		msg := fmt.Sprintf("工作流执行错误: %v", err)
		onStep(StepEvent{Type: StepError, Action: action, Source: observability.ToolSourceMainReAct, Message: msg})
		return observed(msg)
	}

	// Remember a confirmed target even if execution failed, but not one whose
	// authorization was cancelled or never reached.
	e.lastConfirmationAcceptedThisCall = result.ConfirmationAccepted()

	// Read back and describe the exact contract the user confirmed.
	finalParams := workflowFinalParams(result, args)

	if !result.Success {
		result.Message = security.RedactKnownSecretsInText(result.Message, workflowSecretValues(finalParams))
		missing := result.MissingSlots
		if len(missing) > 0 {
			payload, _ := json.Marshal(security.RedactForLLM(map[string]any{
				"success": false, "operation": action, "missing_slots": missing,
				"message": result.Message,
			}))
			onStep(StepEvent{Type: StepToolResult, Action: action, Source: observability.ToolSourceMainReAct, Message: "工作流返回结构化缺参结果，由中央 Agent 结合上下文处理"})
			return observed(string(payload))
		}
		if reply, ok := e.authorizedWriteFailureReply(ctx, action, finalParams, result); ok {
			// The upstream call may have committed before returning an error. A
			// fresh readback, not the pre-write session snapshot, now owns the
			// answer. Never invite an automatic retry of a result that may exist.
			e.markRegistryInvalidated(action)
			onStep(blockedStepEvent(action, observability.ToolSourceMainReAct, nil, reply, result.Err))
			return deterministicReply(reply)
		}
		if action == "CreateInstanceWorkflow" && !result.ConfirmationAccepted() && result.Message != "用户取消了操作" {
			// No creation was authorized. Return the actual validation failure to
			// the Agent; it can inspect alternatives or ask the user without a
			// second, server-owned image/GPU selection loop.
			if msg, ok := friendlyMessageFromText(result.Message); ok {
				result.Message = msg
			}
			onStep(blockedStepEvent(action, observability.ToolSourceMainReAct, nil, result.Message, result.Err))
			payload, _ := json.Marshal(result)
			return observed(string(payload))
		}
		if msg, ok := friendlyMessageFromText(result.Message); ok {
			onStep(blockedStepEvent(action, observability.ToolSourceMainReAct, nil, msg, result.Err))
			return deterministicReply(msg)
		}
	}

	// An unresolved confirmation and an explicit decline share this workflow
	// result, so state only that the operation was not executed.
	if !result.Success && result.Message == "用户取消了操作" {
		return deterministicReply(fmt.Sprintf("好的，%s操作未执行。如需继续，请重新发送指令并确认。", friendlyActionName(action)))
	}

	// An authorized create failure may have affected the instance. Keep its
	// outcome explicit instead of selecting replacements or retrying here.
	if !result.Success && action == "CreateInstanceWorkflow" {
		reply := createWorkflowFailureReply(result.Message, result.Err)
		onStep(blockedStepEvent(action, observability.ToolSourceMainReAct, nil, reply, result.Err))
		return deterministicReply(reply)
	}
	if !result.Success && action == "CreateCFSWorkflow" {
		reply := cfsWorkflowFailureReply(result.Message)
		onStep(blockedStepEvent(action, observability.ToolSourceMainReAct, nil, reply, nil))
		return deterministicReply(reply)
	}
	if !result.Success && action == "CloneCustomImageWorkflow" {
		reply := strings.TrimSpace(workflowStepPrefixRE.ReplaceAllString(result.Message, ""))
		if reply == "" {
			reply = "克隆自制镜像没有成功，请核对源镜像状态和目标可用区后重试。"
		}
		onStep(blockedStepEvent(action, observability.ToolSourceMainReAct, nil, reply, nil))
		return deterministicReply(reply)
	}
	if !result.Success && action == "ReinstallInstanceWorkflow" {
		reply := strings.TrimSpace(workflowStepPrefixRE.ReplaceAllString(result.Message, ""))
		if reply == "" {
			reply = "重装系统没有执行，请核对实例状态和目标镜像后重试。"
		}
		onStep(blockedStepEvent(action, observability.ToolSourceMainReAct, nil, reply, nil))
		return deterministicReply(reply)
	}

	if result.Success && result.MutationCommitted {
		e.markRegistryInvalidated(action)
		e.recordCreatedInstanceReferent(action, finalParams, result)
		// Record the commit BEFORE choosing how to narrate it. From here the write
		// is irreversible upstream, so every later exit — including one where the
		// model never speaks again — has to be able to say so.
		result.Message = committedWriteFallbackReply(action, finalParams, result)
		e.committedWriteRepliesThisTurn = append(e.committedWriteRepliesThisTurn, result.Message)
		// A create may have committed upstream without matching the confirmed
		// contract: the readback can be missing, initialization can fail, or the
		// served spec can differ from the sealed card. Return those exceptional
		// states deterministically. Normal asynchronous initialization remains a
		// successful creation and can continue through the ordinary narration.
		if action == "CreateInstanceWorkflow" {
			if reply, mustReturnDeterministically := createInstanceDeliveryReply(result); mustReturnDeterministically {
				return deterministicReply(reply)
			}
		}
		// Finishing an action does not finish the user's task. Return its actual
		// outcome (including pending asynchronous work) to the same Agent loop;
		// any subsequent write gets a fresh workflow context and confirmation.
	}
	b, _ := json.Marshal(result)
	return observed(string(b))
}

// workflowRequiresInstanceTarget reports whether an action's mutating step
// operates on an EXISTING instance and so must arrive carrying a non-empty
// UHostId (enforced by the fail-safe guard in executeResolvedWorkflow). The
// answer is DERIVED from the action catalog — an operation requires an instance
// target iff its OperationSpec has a Required field whose TargetKind is
// "instance" — rather than hand-maintained as a workflow-name switch that must
// be kept in sync every time a workflow is added (the declarative-from-spec
// convergence; mirrors operationSupportsGuidedIntake). Account/storage creation
// workflows (create/CFS/net-optimizer) require no existing instance, so they have
// no required instance-target field and derive false. On a catalog build error or
// an unknown action it fails CLOSED (returns true → require target), preserving
// the old switch's `default: true` fail-safe.
func workflowRequiresInstanceTarget(action string) bool {
	catalog, err := defaultActionCatalog()
	if err != nil {
		return true
	}
	spec, ok := catalog.Lookup(action)
	if !ok {
		return true
	}
	for _, field := range spec.Fields {
		if field.Required && field.TargetKind == "instance" {
			return true
		}
	}
	return false
}

// deterministicWorkflowReply summarizes a committed lifecycle operation for the
// tool result and the interruption fallback. Successful results still return to
// the agent loop so it can finish the rest of the user's request.
func deterministicWorkflowReply(action string, args map[string]any) (string, bool) {
	uhost, _ := args["UHostId"].(string)
	switch action {
	case "RebootInstanceWorkflow":
		return fmt.Sprintf("✅ 已为实例 %s 执行重启。", uhost), true
	case "StartInstanceWorkflow":
		mode := strings.ToLower(strings.TrimSpace(fmt.Sprint(args["StartMode"])))
		legacySpec := strings.ToUpper(strings.TrimSpace(fmt.Sprint(args["WithoutGpuSpec"])))
		switch {
		case mode == "cpu_only_2c4g" || legacySpec == "A":
			return fmt.Sprintf("✅ 已将实例 %s 改为 2核/4GB 的 CPU-only 规格并执行开机，启动需要一点时间，请稍后查看。", uhost), true
		case mode == "cpu_only_8c16g" || legacySpec == "B":
			return fmt.Sprintf("✅ 已将实例 %s 改为 8核/16GB 的 CPU-only 规格并执行开机，启动需要一点时间，请稍后查看。", uhost), true
		}
		return fmt.Sprintf("✅ 已为实例 %s 执行开机，启动需要一点时间，请稍后查看。", uhost), true
	case "RenameInstanceWorkflow":
		if name, _ := args["Name"].(string); name != "" {
			return fmt.Sprintf("✅ 已将实例 %s 重命名为「%s」。", uhost, name), true
		}
		return fmt.Sprintf("✅ 已重命名实例 %s。", uhost), true
	case "ResetPasswordWorkflow":
		return fmt.Sprintf("✅ 已为实例 %s 重置密码。出于安全考虑，密码不会在对话中回显。", uhost), true
	case "ReinstallInstanceWorkflow":
		return fmt.Sprintf("✅ 已为实例 %s 发起重装。登录凭据由平台沿用或按目标镜像类型生成，请以控制台显示为准。", uhost), true
	case "ResizeCFSWorkflow":
		cfsID := strings.TrimSpace(fmt.Sprint(args["CfsId"]))
		size, _ := firstNumberAny(args, "Size")
		if size > 0 {
			return fmt.Sprintf("✅ 已将 CFS %s 扩容到 %.0fGB。", cfsID, size), true
		}
		return fmt.Sprintf("✅ 已完成 CFS %s 扩容。", cfsID), true
	default:
		return "", false
	}
}

func switchChargeTypeWorkflowReply(action string, params map[string]any, result *workflow.Result) (string, bool) {
	if action != "SwitchChargeTypeWorkflow" {
		return "", false
	}
	id := strings.TrimSpace(fmt.Sprint(params["UHostId"]))
	target := strings.TrimSpace(fmt.Sprint(params["DestChargeType"]))
	targetLabel := workflow.ChargeTypeLabel(target)
	if result == nil || result.Data == nil {
		return fmt.Sprintf("已提交实例 %s 切换为%s的请求，但未能回读当前计费方式。请稍后查看平台账单，请勿重复提交。", id, targetLabel), true
	}
	verified, _ := result.Data["Verified"].(bool)
	if verified {
		return fmt.Sprintf("✅ 已将实例 %s 的计费方式切换为%s，并已通过实时回读确认。", id, targetLabel), true
	}
	readbackAvailable, _ := result.Data["ReadbackAvailable"].(bool)
	if !readbackAvailable {
		return fmt.Sprintf("已提交实例 %s 切换为%s的请求，但未能回读当前计费方式。请稍后查看平台账单，请勿重复提交。", id, targetLabel), true
	}
	observed := strings.TrimSpace(fmt.Sprint(result.Data["ObservedChargeType"]))
	if observed == "" || observed == "<nil>" {
		return fmt.Sprintf("已提交实例 %s 切换为%s的请求，但实时回读没有返回计费方式。请稍后查看平台账单，请勿重复提交。", id, targetLabel), true
	}
	return fmt.Sprintf("已提交实例 %s 切换为%s的请求；实时回读仍显示为%s，尚未确认切换完成。请稍后查看平台账单，请勿重复提交。",
		id, targetLabel, workflow.ChargeTypeLabel(observed)), true
}

// committedWriteFallbackReply is the sentence the user gets when a write has
// landed and the model is not available to narrate it. It must be composable
// with no model call and no further upstream call — the situations that reach
// it are exactly the ones where those are what broke.
//
// It reuses deterministicWorkflowReply where that already has a sentence, so
// the lifecycle workflows read identically whether or not the turn survived.
// The data-bearing ones fall through to their result payload: for create, the
// ids the workflow already returned. The generic branch names the action rather
// than claiming a specific effect — "已执行成功" with nothing to point at is the
// weakest true statement available, and a weak truth beats a confident guess.
func committedWriteFallbackReply(action string, params map[string]any, result *workflow.Result) string {
	if action == "StopInstanceWorkflow" {
		return stopInstanceWorkflowReply(params)
	}
	if reply, ok := scheduledShutdownWorkflowReply(action, params, result); ok {
		return reply
	}
	if action == "CloneCustomImageWorkflow" {
		return cloneCustomImageWorkflowReply(result)
	}
	if reply, ok := switchChargeTypeWorkflowReply(action, params, result); ok {
		return reply
	}
	if reply, ok := deterministicWorkflowReply(action, params); ok {
		return reply
	}
	if action == "CreateCustomImageWorkflow" {
		return customImageWorkflowReply(result)
	}
	if action == "CreateInstanceWorkflow" {
		if reply, _ := createInstanceDeliveryReply(result); reply != "" {
			return reply
		}
	}
	if ids := committedInstanceIDs(result); len(ids) > 0 {
		return fmt.Sprintf("✅ 已创建实例 %s。", strings.Join(ids, "、"))
	}
	return fmt.Sprintf("✅ %s已执行成功。", friendlyActionName(action))
}

func stopInstanceWorkflowReply(params map[string]any) string {
	uhost := strings.TrimSpace(fmt.Sprint(params["UHostId"]))
	return fmt.Sprintf("已向实例 %s 提交关机请求，平台正在处理。请稍后查看最终状态，请勿重复提交。", uhost)
}

// createInstanceDeliveryReply describes what the platform has actually
// delivered after a confirmed create. Result.Success remains the write outcome:
// once UHostIds exist, changing it to false would invite a duplicate billable
// create. The bool instead says whether an incomplete, failed or mismatched
// delivery must bypass model narration. Normal post-create initialization is
// successful and keeps the ordinary narration path.
func createInstanceDeliveryReply(result *workflow.Result) (string, bool) {
	ids := committedInstanceIDs(result)
	if len(ids) == 0 {
		if result != nil && result.Success {
			return "创建请求已由平台处理，但没有返回实例 ID，当前无法确认创建结果。请勿重复创建，请稍后在控制台核对实例列表。", true
		}
		return "", false
	}
	idText := strings.Join(ids, "、")
	data := result.Data
	dataDiskNote, hasDataDisk := createDataDiskDeliveryNote(data)
	finish := func(reply string, deterministic bool) (string, bool) {
		if !hasDataDisk {
			return reply, deterministic
		}
		return strings.TrimSpace(reply + " " + dataDiskNote), true
	}
	readback, _ := data["ActualReadbackAvailable"].(bool)
	observed, _ := data["Observed"].([]map[string]any)
	if !readback || len(observed) != len(ids) {
		return finish(fmt.Sprintf("创建接口已返回实例 ID：%s，但尚未取得完整的创建后状态，暂不能确认实例是否可用。请勿重复创建，请稍后在控制台查看实例状态。", idText), true)
	}

	hasInstallFail := false
	hasInitializing := false
	hasStarting := false
	unexpectedState := ""
	for _, row := range observed {
		state := strings.TrimSpace(fmt.Sprint(row["State"]))
		if state == "<nil>" {
			state = ""
		}
		switch strings.ToLower(state) {
		case "running":
		case "initializing", "installing", "install":
			hasInitializing = true
		case "starting":
			hasStarting = true
		case "install fail":
			hasInstallFail = true
		default:
			if unexpectedState == "" {
				unexpectedState = state
			}
		}
	}

	mismatchText := createSpecMismatchText(data["SpecMismatch"])
	if hasInstallFail {
		reply := fmt.Sprintf("实例记录已创建（ID：%s），但初始化失败（Install Fail），目前不可用", idText)
		if mismatchText != "" {
			reply += "；创建后规格也与确认内容不一致：" + mismatchText
		}
		return finish(reply+"。本次不能视为交付成功，请勿重复创建。", true)
	}
	if mismatchText != "" {
		return finish(fmt.Sprintf("实例已创建（ID：%s），但创建后规格与确认内容不一致：%s。本次不能视为按确认规格交付，请勿重复创建。", idText, mismatchText), true)
	}
	if !createSpecReadbackComplete(data, observed) {
		return finish(fmt.Sprintf("实例已创建（ID：%s），但创建后规格回读不完整，暂不能确认是否按确认内容交付。请勿重复创建，请稍后在控制台核对实例规格。", idText), true)
	}
	if unexpectedState != "" {
		return finish(fmt.Sprintf("实例记录已创建（ID：%s），当前状态为%s，尚未确认可用。请勿重复创建，请稍后查看实例状态。",
			idText, readprojection.ResourceStateLabel(unexpectedState)), true)
	}
	if hasInitializing || hasStarting {
		phase := "正在初始化"
		if hasStarting && !hasInitializing {
			phase = "正在启动"
		} else if hasStarting {
			phase = "正在初始化或启动"
		}
		return finish(fmt.Sprintf("✅ 已创建实例 %s，%s，进入运行状态后即可使用。", idText, phase), false)
	}
	return finish(fmt.Sprintf("✅ 已创建实例 %s。", idText), false)
}

func createDataDiskDeliveryNote(data map[string]any) (string, bool) {
	if data == nil {
		return "", false
	}
	delivery, ok := data["DataDiskDelivery"].(map[string]any)
	if !ok {
		return "", false
	}
	switch strings.ToLower(strings.TrimSpace(fmt.Sprint(delivery["State"]))) {
	case "verified":
		return "创建时请求的数据盘已由创建后回读确认挂载完成。", true
	default:
		return "创建时请求的数据盘由平台异步创建并挂载，本次回读尚未确认完成；请稍后查看磁盘状态，请勿重复创建实例或数据盘。", true
	}
}

func createSpecReadbackComplete(data map[string]any, observed []map[string]any) bool {
	intended, ok := data["Intended"].(map[string]any)
	if !ok {
		return false
	}
	for _, row := range observed {
		for _, field := range []string{"CPU", "Memory", "GPU", "GpuType", "Zone"} {
			if !createSpecValuePresent(intended[field]) || !createSpecValuePresent(row[field]) {
				return false
			}
		}
	}
	return true
}

func createSpecValuePresent(value any) bool {
	if value == nil {
		return false
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text) != ""
	}
	if number, ok := numberAny(value); ok {
		return number != 0
	}
	return true
}

func createSpecMismatchText(raw any) string {
	rows, _ := raw.([]map[string]any)
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		field := strings.TrimSpace(fmt.Sprint(row["Field"]))
		if field == "" || field == "<nil>" {
			continue
		}
		label := field
		switch field {
		case "Memory":
			label = "内存"
		case "GpuType":
			label = "GPU 型号"
		case "GPU":
			label = "GPU 卡数"
		case "Zone":
			label = "可用区"
		}
		parts = append(parts, fmt.Sprintf("%s 确认 %s、实际 %s",
			label, createSpecDisplayValue(field, row["Intended"]), createSpecDisplayValue(field, row["Observed"])))
	}
	return strings.Join(parts, "；")
}

func createSpecDisplayValue(field string, value any) string {
	if field == "Memory" {
		if mb, ok := numberAny(value); ok && mb > 0 && mb == float64(int64(mb)) && int64(mb)%1024 == 0 {
			return fmt.Sprintf("%.0fGB", mb/1024)
		}
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

// customImageWorkflowReply describes the server-side state transition actually
// guaranteed by CreateCompShareCustomImage. Upstream creates the image record in
// Making and advances it asynchronously, so this must never call a successful
// create a completed or usable image.
func customImageWorkflowReply(result *workflow.Result) string {
	imageID := ""
	sourceNote := ""
	if result != nil && result.Data != nil {
		imageID, _ = result.Data["CompShareImageId"].(string)
		imageID = strings.TrimSpace(imageID)
		sourceNote, _ = result.Data["SourceInstanceNote"].(string)
	}
	if imageID != "" {
		return fmt.Sprintf("✅ 已发起自制镜像制作（ID: %s）。镜像已进入制作流程（初始状态为 Making）；变为 Available 后才能用于创建实例、共享或克隆。%s", imageID, sourceNote)
	}
	return "✅ 已发起自制镜像制作。镜像已进入制作流程（初始状态为 Making）；变为 Available 后才能用于创建实例、共享或克隆。" + sourceNote
}

// committedWriteNarrationFailedNote tells the user the missing half explicitly.
// Without it the reply reads like a complete answer that simply chose to say
// very little, and the user cannot tell that the next-steps guidance they
// normally get was lost rather than withheld.
const committedWriteNarrationFailedNote = "（本次未能生成完整说明；上述写操作已由平台处理，请勿重复提交。）"

// committedWriteRecoveryReply renders this turn's committed writes as a final
// answer, or reports that there were none. Callers use the bool: an empty
// record must fall through to the normal error path rather than produce a
// cheerful empty confirmation.
func (e *Engine) committedWriteRecoveryReply() (string, bool) {
	if len(e.committedWriteRepliesThisTurn) == 0 {
		return "", false
	}
	return strings.Join(e.committedWriteRepliesThisTurn, "\n") + "\n\n" + committedWriteNarrationFailedNote, true
}

// committedInstanceIDs reads the created instance ids out of a workflow result.
// The create workflow already publishes them as ResultData["UHostIds"]
// (internal/workflow/create_instance.go::createInstanceResultData), so this
// re-reads the workflow's own output rather than re-deriving it — there is no
// second source that could disagree.
func committedInstanceIDs(result *workflow.Result) []string {
	if result == nil || result.Data == nil {
		return nil
	}
	raw, ok := result.Data["UHostIds"]
	if !ok {
		return nil
	}
	var out []string
	switch ids := raw.(type) {
	case []string:
		for _, id := range ids {
			if s := strings.TrimSpace(id); s != "" {
				out = append(out, s)
			}
		}
	case []any:
		for _, id := range ids {
			if s := strings.TrimSpace(fmt.Sprint(id)); s != "" && s != "<nil>" {
				out = append(out, s)
			}
		}
	}
	return out
}

func workflowSecretValues(args map[string]any) []string {
	if args == nil {
		return nil
	}
	var out []string
	for _, key := range []string{"Password", "password", "NewPassword", "LoginPassword"} {
		raw, ok := args[key]
		if !ok {
			continue
		}
		secret := strings.TrimSpace(fmt.Sprint(raw))
		if secret == "" {
			continue
		}
		out = append(out, secret)
		out = append(out, base64.StdEncoding.EncodeToString([]byte(secret)))
	}
	return out
}

// workflowStepPrefixRE strips the technical step wrapper the workflow engine adds
// to BuildArgs / executor failures ("步骤「检查库存」参数构建失败: …") so the user sees
// only the grounded reason. The inner message is what carries real information
// (available types, sold-out spec, etc.).
var workflowStepPrefixRE = regexp.MustCompile(`^步骤「[^」]*」(?:参数构建失败|执行失败)[：:]\s*`)

// createWorkflowFailureReply explains a failed authorized create without
// choosing a replacement or submitting another write.
func createWorkflowFailureReply(message string, err error) string {
	if isImageUnavailableError(err) {
		return "抱歉，创建实例没有成功：您指定的镜像在当前可用区暂不可用。请更换镜像名称重试，或在控制台创建页选择该可用区支持的镜像。"
	}
	if deployment.ClassifyCreateFailure(message).Kind == deployment.FailureImageZoneNotAdapted {
		return "抱歉，创建实例没有成功：您选择的镜像在当前可用区暂未适配。请更换镜像，或选择其他可用区后重试。"
	}
	msg := workflowStepPrefixRE.ReplaceAllString(strings.TrimSpace(message), "")
	msg = strings.TrimSpace(msg)
	if msg == "" {
		msg = "未能创建实例，请稍后重试或更换机型/配置。"
	}
	return "抱歉，创建实例没有成功：" + msg
}

// workflowFinalParams uses sealed params only after execution was actually authorized.
// Earlier guided steps may also create a contract, but do not authorize the final write.
func workflowFinalParams(result *workflow.Result, args map[string]any) map[string]any {
	if result.Contract == nil {
		return args
	}
	if result.Success {
		return result.Contract.BusinessParams
	}
	if result.Failure != nil && result.Failure.ExecutionAuthorized {
		return result.Contract.BusinessParams
	}
	return args
}

func cfsWorkflowFailureReply(message string) string {
	msg := workflowStepPrefixRE.ReplaceAllString(strings.TrimSpace(message), "")
	msg = strings.TrimSpace(msg)
	if msg == "" {
		msg = "未能创建 CFS，请稍后重试或更换可用区。"
	}
	return "抱歉，CFS 创建没有成功：" + msg
}
