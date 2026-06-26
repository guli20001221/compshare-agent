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

	if pref, err := e.extractCreatePreference(ctx, userMsg, intent.IntentCreateInstance); err == nil && pref != nil {
		e.createPreferenceThisTurn = pref
		if createPreferenceHasWorkload(*pref) {
			deployDispatch := dispatch
			deployDispatch.result.Plan.Intent = intent.IntentDeployModel
			matchUserMsg := deployMessageWithCreatePreference(userMsg, *pref)
			return e.runDeployModel(ctx, deployDispatch, userMsg, userMsg, matchUserMsg, onStep)
		}
	}

	args := map[string]any{}
	availResult := e.querySafeRead(ctx, "DescribeAvailableCompShareInstanceTypes", map[string]any{})
	if gpu := extractDeployGPUFromCatalog(userMsg, availResult); gpu != "" {
		args["GpuType"] = gpu
		args["Gpu"] = float64(1)
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

func createPreferenceHasWorkload(pref CreatePreferenceExtractionResult) bool {
	return strings.TrimSpace(pref.WorkloadPref) != ""
}

func createInstanceShouldNotOpenCreateCard(userMsg string) bool {
	text := strings.TrimSpace(userMsg)
	return text == "" || hardwareAdviceRE.MatchString(text) || hardwarePriceQueryRE.MatchString(text) || deployIsAdviceOnly(text)
}

func createInstanceNonCommandReply() string {
	return "这是价格、建议或操作方法类问题，我不会直接创建实例。请直接说明你想查询价格、比较配置，或明确说“现在帮我创建”后，我再进入确认卡。"
}
