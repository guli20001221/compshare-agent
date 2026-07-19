package tools

import openai "github.com/sashabaranov/go-openai"

const ProposeActionName = "ProposeAction"
const UpdateTaskStateName = "UpdateTaskState"

// ShadowCapabilityDefinitions are fully executable contracts that are not yet
// advertised to production model windows. P6 promotes them by changing Stage;
// keeping them in the same registry lets policy and execution tests run before
// the routing cutover.
var ShadowCapabilityDefinitions = []CapabilityDefinition{
	{
		Stage: CapabilityStageShadow,
		Tool: openai.Tool{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{
			Name: ProposeActionName,
			// Base template only — never shown to the model. The window exposes one
			// Request<Operation> variant per write op (dispatch_window.go), each
			// carrying the single model-visible contract (proposalInvocationContract)
			// plus that operation's field schema. The high-level behavior rule lives
			// once in the system prompt (segmentCentralAgentBehavior).
			Description: "写操作动作建议的基础模板；每个写操作以 Request<Operation> 变体暴露并携带该操作的字段契约。本工具不直接执行。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"operation": map[string]any{"type": "string"},
					"slots": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"name":  map[string]any{"type": "string"},
								"value": map[string]any{},
							},
							"required": []string{"name", "value"},
						},
					},
				},
				"required": []string{"operation", "slots"},
			},
		}},
	},
	{
		Stage: CapabilityStageShadow,
		Tool: openai.Tool{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{
			Name:        UpdateTaskStateName,
			Description: "提交结构化任务关系变更。只更新对话任务状态，不执行任何平台操作。relation 明确表示继续、替换、完成或清除旧任务。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"relation": map[string]any{"type": "string", "enum": []string{"continue", "replace", "complete", "clear"}},
					"task": map[string]any{"type": "object", "properties": map[string]any{
						"goal": map[string]any{"type": "string"}, "intent": map[string]any{"type": "string"}, "workflow": map[string]any{"type": "string"},
						"stage": map[string]any{"type": "string"}, "constraints": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"decisions":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"missing_slots": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					}},
				},
				"required": []string{"relation"},
			},
		}},
	},
}
