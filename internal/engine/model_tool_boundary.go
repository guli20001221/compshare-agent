package engine

import (
	"context"

	openai "github.com/sashabaranov/go-openai"

	"github.com/compshare-agent/internal/observability"
	"github.com/compshare-agent/internal/tools"
)

// executeModelTool is the authorization boundary between the exact tool window
// sent to the provider and the larger internal action registry. Workflows and
// diagnosis chains intentionally call raw platform actions through their
// internal origins; a model tool call may use only a name advertised in this
// request. Keeping the check next to the response that used the window avoids a
// mutable engine-wide allowlist and remains correct when later rounds shed
// single-shot or budget-exhausted tools.
func (e *Engine) executeModelTool(ctx context.Context, tc openai.ToolCall, toolWindow []openai.Tool, onStep func(StepEvent)) string {
	action := tc.Function.Name
	// Action is sometimes projected to a stable internal operation (for example,
	// RequestStopInstance is recorded as ProposeAction). Preserve the exact model-
	// selected function on the first emitted event only, so traces can measure the
	// advertised tool window without duplicating that selection onto nested API
	// calls made by capabilities or workflows.
	selectionRecorded := false
	recordSelection := func(ev StepEvent) {
		if !selectionRecorded && ev.Action != "" {
			switch ev.Type {
			case StepToolCall, StepToolResult, StepError, StepBlocked:
				ev.SelectedFunctionName = action
				selectionRecorded = true
			}
		}
		onStep(ev)
	}
	if !toolListContainsFunction(toolWindow, action) {
		const message = "当前轮次未开放该工具，已拒绝执行"
		agentResult := tools.AgentToolFailure(
			action, nil, "TOOL_NOT_ALLOWED", message, tools.AgentToolMeta{},
		)
		recordSelection(StepEvent{
			Type: StepBlocked, Action: action, Source: observability.ToolSourceMainReAct,
			Message: message, ErrorCode: agentResult.Error.Code,
		})
		return tools.MarshalAgentToolResult(agentResult)
	}
	return e.executeTool(ctx, tc, recordSelection)
}
