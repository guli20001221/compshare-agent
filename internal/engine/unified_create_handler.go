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
		e.clearCreateFamilyCarry()
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
	if e.deferTaskCarryThisTurn {
		return "", false
	}
	if !ContextContinuationEnabled() {
		return "", false
	}
	frame, ok := e.activeContextFrame(time.Now())
	if !ok || !contextFrameCreateFamily(frame.Kind) {
		return "", false
	}
	decision, err := e.resolveContextContinuation(ctx, userMsg, dispatch.result.Plan.Intent, frame)
	if err != nil || decision == nil {
		return "", false
	}
	switch decision.Decision {
	case ContextContinuationNew:
		e.clearContextFrame()
		return "", false
	case ContextContinuationClear:
		e.clearContextFrame()
		return "", false
	case ContextContinuationClarify:
		if decision.Clarify != "" {
			return e.deployReply(dispatch.result, dispatch.latency, decision.Clarify)
		}
		return "", false
	case ContextContinuationContinue:
	default:
		return "", false
	}

	nextFrame, clarify := e.applyContextContinuationDecision(ctx, userMsg, frame, *decision)
	if clarify != "" {
		return e.deployReply(dispatch.result, dispatch.latency, clarify)
	}
	nextFrame = dropStaleDeployPayload(nextFrame, dispatch.result.Plan.Intent, *decision)
	if contextFramesEquivalent(frame, nextFrame) {
		if strings.TrimSpace(decision.Clarify) != "" {
			return e.deployReply(dispatch.result, dispatch.latency, strings.TrimSpace(decision.Clarify))
		}
		return "", false
	}
	msg := mergeContextFrameCreateMessage(nextFrame, userMsg, nextFrame.ZoneLabel)
	if nextFrame.Kind == ContextFrameKindDeploy || nextFrame.Workload != "" || nextFrame.ImagePref != "" || nextFrame.ImageSource != "" {
		deployDispatch := dispatch
		deployDispatch.result.Plan.Intent = intent.IntentDeployModel
		return e.runDeployModel(ctx, deployDispatch, userMsg, msg, msg, onStep)
	}
	createDispatch := dispatch
	createDispatch.result.Plan.Intent = intent.IntentCreateInstance
	return e.tryCreateInstanceWithResolvedFrame(ctx, createDispatch, userMsg, nextFrame, nextFrame.Zone, onStep)
}

// dropStaleDeployPayload removes the deploy-specific payload — the workload, the image
// preference, the image source — that a frame INHERITED from a previous task, when the
// current turn is a plain hardware create.
//
// applyContextContinuationDecision opens with `next := frame`, a full copy, and then
// overwrites only the fields the resolver explicitly named. That is exactly right for a real
// continuation (「换成 A100」 must keep the workload it is changing the GPU for) and exactly
// wrong for a new create. A failed 「部署 DeepSeek R1」 leaves a frame carrying
// Workload="DeepSeek R1", so the very next 「创建一台 4090」 — which the router classified,
// confidently, as create_instance with no workload at all — comes out of the merge still
// carrying DeepSeek. The caller then reads `nextFrame.Workload != ""`, rewrites the intent to
// deploy_model, and hands it to runDeployModel. The user asked for a bare GPU box and the
// agent tried to redeploy the model that had just failed on them.
//
// The frame is evidence about what the user wanted BEFORE. The router's intent is evidence
// about what they want NOW. When the two disagree about the KIND of task, now wins — and a
// workload nobody mentioned this turn is not context, it is contamination.
//
// Only inherited values are dropped. Anything the resolver named THIS turn survives, so a
// genuine 「用 vLLM 部署」 still becomes a deploy. GPU and zone are also kept: those are the
// legitimately reusable half of the frame ("再来一台" should still mean 4090 in 华北一).
func dropStaleDeployPayload(next ContextFrame, routerIntent intent.Intent, decision ContextContinuationDecision) ContextFrame {
	if routerIntent != intent.IntentCreateInstance {
		return next
	}
	if strings.TrimSpace(decision.WorkloadPref) == "" {
		next.Workload = ""
	}
	if strings.TrimSpace(decision.ImagePref) == "" {
		next.ImagePref = ""
	}
	if strings.TrimSpace(decision.ImageSource) == "" {
		next.ImageSource = ""
	}
	if next.Workload == "" && next.ImagePref == "" && next.ImageSource == "" {
		next.Kind = ContextFrameKindCreate
	}
	return next
}

func (e *Engine) applyContextContinuationDecision(ctx context.Context, userMsg string, frame ContextFrame, decision ContextContinuationDecision) (ContextFrame, string) {
	next := frame
	gpuPref := strings.TrimSpace(decision.GPUPref)
	if gpuPref == "" {
		availResult := e.querySafeRead(ctx, "DescribeAvailableCompShareInstanceTypes", map[string]any{})
		gpuPref = extractDeployGPUFromCatalog(userMsg, availResult)
	}
	if gpu := gpuPref; gpu != "" {
		availResult := e.querySafeRead(ctx, "DescribeAvailableCompShareInstanceTypes", map[string]any{})
		resolved := extractDeployGPUFromCatalog(gpu, availResult)
		if resolved == "" {
			return frame, "没有在当前可售机型里找到你提到的 GPU：" + gpu + "。请换一个平台可售的 GPU 型号。"
		}
		next.GPU = resolved
	}
	zonePref := strings.TrimSpace(decision.ZonePref)
	if zonePref == "" {
		if zone, clarify := e.resolveRequestedZone(ctx, userMsg); zone != "" {
			zonePref = zone
		} else if clarify != "" {
			return frame, clarify
		}
	}
	if zonePref != "" {
		zone, clarify := e.resolveRequestedZone(ctx, zonePref)
		if zone == "" && strings.TrimSpace(userMsg) != "" && strings.TrimSpace(userMsg) != zonePref {
			if fallbackZone, fallbackClarify := e.resolveRequestedZone(ctx, userMsg); fallbackZone != "" {
				zone = fallbackZone
				clarify = ""
			} else if clarify == "" {
				clarify = fallbackClarify
			}
		}
		if clarify != "" {
			return frame, clarify
		}
		if zone == "" {
			return frame, "没有在当前支持区里找到你提到的可用区：" + zonePref + "。请换一个控制台可选的可用区。"
		}
		next.Zone = zone
		next.ZoneLabel = e.zoneDisplayName(ctx, zone)
	}
	if imagePref := strings.TrimSpace(decision.ImagePref); imagePref != "" {
		next.ImagePref = imagePref
	}
	if imageSource := strings.TrimSpace(decision.ImageSource); imageSource != "" {
		next.ImageSource = imageSource
	}
	if workload := strings.TrimSpace(decision.WorkloadPref); workload != "" {
		next.Workload = workload
		next.Kind = ContextFrameKindDeploy
	}
	if next.Zone != "" && next.ZoneLabel == "" {
		next.ZoneLabel = e.zoneDisplayName(ctx, next.Zone)
	}
	return next, ""
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
		e.clearCreateFamilyCarry()
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

func contextFramesEquivalent(a, b ContextFrame) bool {
	return strings.TrimSpace(a.Kind) == strings.TrimSpace(b.Kind) &&
		strings.TrimSpace(a.GPU) == strings.TrimSpace(b.GPU) &&
		strings.TrimSpace(a.ImagePref) == strings.TrimSpace(b.ImagePref) &&
		strings.TrimSpace(a.ImageSource) == strings.TrimSpace(b.ImageSource) &&
		strings.TrimSpace(a.Workload) == strings.TrimSpace(b.Workload) &&
		strings.TrimSpace(a.Zone) == strings.TrimSpace(b.Zone)
}

func (e *Engine) recordCreateContextFrameFromCreateAttempt(userMsg string, plan intent.IntentRoute, args map[string]any, reply string) {
	if !ContextContinuationEnabled() {
		return
	}
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
