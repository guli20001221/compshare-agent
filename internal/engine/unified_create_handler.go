package engine

import (
	"context"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/observability"
)

func (e *Engine) tryCreateInstance(ctx context.Context, dispatch routerDispatchResult, userMsg string, onStep func(StepEvent)) (string, bool) {
	if !unifiedCreateOn {
		return "", false
	}
	if dispatch.result.Plan.Intent == intent.IntentDeployModel {
		return e.tryDeployModel(ctx, dispatch, userMsg, onStep)
	}
	if dispatch.result.Plan.Intent != intent.IntentCreateInstance {
		return "", false
	}

	if createInstanceShouldNotOpenCreateCard(userMsg) {
		return e.deployReply(dispatch.result, dispatch.latency, createInstanceNonCommandReply())
	}
	var pref *CreatePreferenceExtractionResult
	if createPreferenceExtractionOn {
		if extracted, err := e.extractCreatePreference(ctx, userMsg, intent.IntentCreateInstance); err == nil && extracted != nil {
			pref = extracted
			e.createPreferenceThisTurn = extracted
			if createPreferenceNeedsImageMatcher(*extracted) {
				deployDispatch := dispatch
				deployDispatch.result.Plan.Intent = intent.IntentDeployModel
				matchUserMsg := deployMessageWithCreatePreference(userMsg, *extracted)
				return e.runDeployModel(ctx, deployDispatch, userMsg, userMsg, matchUserMsg, onStep)
			}
		}
	}

	args := map[string]any{}
	availResult := e.querySafeRead(ctx, "DescribeAvailableCompShareInstanceTypes", map[string]any{})
	if gpu := createInstanceGPUFromPreferenceOrText(userMsg, pref, availResult); gpu != "" {
		args["GpuType"] = gpu
		args["Gpu"] = float64(1)
	}
	if zone, clarify := e.resolveRequestedZone(ctx, userMsg); clarify != "" {
		return e.deployReply(dispatch.result, dispatch.latency, clarify)
	} else if zone != "" {
		args["Zone"] = zone
		args["GuidedZoneLocked"] = true
	}

	const action = "CreateInstanceWorkflow"
	e.emitPlannerTrace(dispatch.result, intent.RouteStatusDispatchedAgent, dispatch.latency)
	args = e.safeExecutor.FilterArgs(action, args)
	onStep(StepEvent{
		Type:   StepToolCall,
		Action: action,
		Source: observability.ToolSourceMainReAct,
		Args:   e.safeExecutor.RedactArgs(action, args),
	})
	raw := e.executeWorkflow(ctx, action, args, onStep)
	reply := workflowDirectReply(action, raw)
	if createAttemptShouldClearContextFrame(reply) {
		e.clearContextFrame()
	} else {
		e.recordCreateContextFrameFromCreateAttempt(userMsg, dispatch.result.Plan, args, reply)
	}
	e.messages = append(e.messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleAssistant,
		Content: reply,
	})
	return reply, true
}

func (e *Engine) tryResumeCreateContextFrame(ctx context.Context, dispatch routerDispatchResult, userMsg string, onStep func(StepEvent)) (string, bool) {
	if !canResumeCreateContextFrameFromIntent(dispatch.result.Plan.Intent) {
		return "", false
	}
	frame, ok := e.activeContextFrame(time.Now())
	if !ok || !contextFrameCreateFamily(frame.Kind) {
		return "", false
	}
	zone, clarify := e.resolveRequestedZone(ctx, userMsg)
	if clarify != "" {
		return e.deployReply(dispatch.result, dispatch.latency, clarify)
	}
	if zone == "" {
		return "", false
	}
	msg := mergeContextFrameCreateMessage(frame, userMsg, e.zoneDisplayName(ctx, zone))
	if frame.Kind == ContextFrameKindDeploy || frame.Workload != "" || frame.ImagePref != "" || frame.ImageSource != "" {
		deployDispatch := dispatch
		deployDispatch.result.Plan.Intent = intent.IntentDeployModel
		return e.runDeployModel(ctx, deployDispatch, userMsg, msg, msg, onStep)
	}
	createDispatch := dispatch
	createDispatch.result.Plan.Intent = intent.IntentCreateInstance
	return e.tryCreateInstanceWithResolvedFrame(ctx, createDispatch, userMsg, frame, zone, onStep)
}

func (e *Engine) tryCreateInstanceWithResolvedFrame(ctx context.Context, dispatch routerDispatchResult, userMsg string, frame ContextFrame, zone string, onStep func(StepEvent)) (string, bool) {
	args := map[string]any{}
	if frame.GPU != "" {
		args["GpuType"] = frame.GPU
		args["Gpu"] = float64(1)
	}
	if zone != "" {
		args["Zone"] = zone
		args["GuidedZoneLocked"] = true
	}
	const action = "CreateInstanceWorkflow"
	e.emitPlannerTrace(dispatch.result, intent.RouteStatusDispatchedAgent, dispatch.latency)
	args = e.safeExecutor.FilterArgs(action, args)
	onStep(StepEvent{Type: StepToolCall, Action: action, Source: observability.ToolSourceMainReAct, Args: e.safeExecutor.RedactArgs(action, args)})
	raw := e.executeWorkflow(ctx, action, args, onStep)
	reply := workflowDirectReply(action, raw)
	if createAttemptShouldClearContextFrame(reply) {
		e.clearContextFrame()
	} else {
		e.recordCreateContextFrameFromCreateAttempt(userMsg, dispatch.result.Plan, args, reply)
	}
	e.messages = append(e.messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: reply})
	return reply, true
}

func mergeContextFrameCreateMessage(frame ContextFrame, userMsg, zoneLabel string) string {
	var parts []string
	if frame.Workload != "" {
		parts = append(parts, "继续部署 "+frame.Workload)
	} else {
		parts = append(parts, "继续创建实例")
	}
	if frame.GPU != "" {
		parts = append(parts, "使用 GPU "+frame.GPU)
	}
	if frame.ImagePref != "" {
		parts = append(parts, "使用镜像偏好 "+frame.ImagePref)
	}
	if frame.ImageSource != "" {
		parts = append(parts, "镜像来源 "+frame.ImageSource)
	}
	if zoneLabel != "" {
		parts = append(parts, "可用区 "+zoneLabel)
	}
	parts = append(parts, "用户追问："+strings.TrimSpace(userMsg))
	return strings.Join(parts, "；")
}

func (e *Engine) recordCreateContextFrameFromCreateAttempt(userMsg string, plan intent.IntentRoute, args map[string]any, reply string) {
	frame := newContextFrame(ContextFrameKindCreate, plan, userMsg, e.userTurn, time.Now())
	frame.Status = ContextFrameStatusFailedRecoverable
	frame.GPU, _ = args["GpuType"].(string)
	frame.Zone, _ = args["Zone"].(string)
	ctx := e.currentCtx
	if ctx == nil {
		ctx = context.Background()
	}
	frame.ZoneLabel = e.zoneDisplayName(ctx, frame.Zone)
	frame.FailureReason = strings.TrimSpace(reply)
	e.setContextFrame(frame)
}

func createAttemptShouldClearContextFrame(reply string) bool {
	return strings.Contains(reply, "创建实例请求已提交") ||
		strings.Contains(reply, "操作未执行") ||
		strings.Contains(reply, "已取消")
}

func createPreferenceNeedsImageMatcher(pref CreatePreferenceExtractionResult) bool {
	return strings.TrimSpace(pref.WorkloadPref) != "" ||
		strings.TrimSpace(pref.ImagePref) != "" ||
		strings.TrimSpace(pref.ImageSource) != "" ||
		strings.TrimSpace(pref.Purpose) != ""
}

func createInstanceGPUFromPreferenceOrText(userMsg string, pref *CreatePreferenceExtractionResult, availResult map[string]any) string {
	if pref != nil {
		if gpu := extractDeployGPUFromCatalog(pref.GPUPref, availResult); gpu != "" {
			return gpu
		}
	}
	return extractDeployGPUFromCatalog(userMsg, availResult)
}

func createInstanceShouldNotOpenCreateCard(userMsg string) bool {
	text := strings.TrimSpace(userMsg)
	return text == ""
}

func createInstanceNonCommandReply() string {
	return "这是价格、建议或操作方法类问题，我不会直接创建实例。请直接说明你想查询价格、比较配置，或明确说“现在帮我创建”后，我再进入确认卡。"
}
