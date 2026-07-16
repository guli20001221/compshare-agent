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
			Name:        ProposeActionName,
			Description: "用户要求写操作时直接提出结构化候选；不要先检索文档来猜必填参数。服务端会返回缺失项。user_explicit 候选的 value 和 quote 必须是用户原文中的同一段文字，类型和单位由 Resolver 转换；内部轮次标识和原文位置由运行时补齐。参数归并通过后仍必须经过权限、确认、日志和执行门。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"operation": map[string]any{"type": "string"},
					"slots": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"name":   map[string]any{"type": "string"},
								"value":  map[string]any{},
								"source": map[string]any{"type": "string", "enum": []string{"user_explicit", "verified_context", "tool_observation", "user_confirmation", "agent_inference"}},
								"evidence": map[string]any{"type": "object", "properties": map[string]any{
									"message_id": map[string]any{"type": "string"}, "context_field": map[string]any{"type": "string"},
									"start": map[string]any{"type": "integer"}, "end": map[string]any{"type": "integer"}, "quote": map[string]any{"type": "string"},
								}},
							},
							"required": []string{"name", "value", "source"},
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
