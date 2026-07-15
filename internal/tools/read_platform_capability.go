package tools

import openai "github.com/sashabaranov/go-openai"

const ReadPlatformCapabilityName = "ReadPlatformCapability"

// ShadowCapabilityDefinitions are fully executable contracts that are not yet
// advertised to production model windows. P6 promotes them by changing Stage;
// keeping them in the same registry lets policy and execution tests run before
// the routing cutover.
var ShadowCapabilityDefinitions = []CapabilityDefinition{{
	Stage: CapabilityStageShadow,
	Tool: openai.Tool{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{
		Name:        ReadPlatformCapabilityName,
		Description: "执行平台只读业务能力，复用现有组合查询、过滤与确定性事实包。capability 使用平台能力名称；slots 只放本轮明确参数。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"capability": map[string]any{"type": "string", "description": "只读能力名称，例如 resource_info、monitor_query、pricing_query、stock_availability、image_list。"},
				"slots":      map[string]any{"type": "object", "description": "该能力的结构化参数；未知字段会被拒绝。"},
			},
			"required": []string{"capability"},
		},
	}},
}}
