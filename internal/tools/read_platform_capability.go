package tools

import openai "github.com/sashabaranov/go-openai"

const ReadPlatformCapabilityName = "ReadPlatformCapability"
const ProposeActionName = "ProposeAction"

// ShadowCapabilityDefinitions are fully executable contracts that are not yet
// advertised to production model windows. P6 promotes them by changing Stage;
// keeping them in the same registry lets policy and execution tests run before
// the routing cutover.
var ShadowCapabilityDefinitions = []CapabilityDefinition{
	{
		Stage: CapabilityStageShadow,
		Tool: openai.Tool{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{
			Name:        ReadPlatformCapabilityName,
			Description: "执行平台只读业务能力，复用现有组合查询、过滤与确定性事实包。capability 使用平台能力名称；slots 只放本轮明确参数。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"capability": map[string]any{"type": "string", "description": "平台只读能力名称。"},
					"slots":      map[string]any{"type": "object", "description": "该能力的结构化参数；未知字段会被拒绝。"},
				},
				"required": []string{"capability"},
			},
		}},
	},
	{
		Stage: CapabilityStageShadow,
		Tool: openai.Tool{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{
			Name:        ProposeActionName,
			Description: "提出结构化写操作候选，仅进行参数归并、冲突和安全条件检查；不会执行任何写操作。每个候选值必须声明来源。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"turn_id":   map[string]any{"type": "string"},
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
				"required": []string{"turn_id", "operation", "slots"},
			},
		}},
	},
}
