package engine

import (
	"context"
	"strings"

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
	e.messages = append(e.messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleAssistant,
		Content: reply,
	})
	return reply, true
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
