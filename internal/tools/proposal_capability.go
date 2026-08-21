package tools

import openai "github.com/sashabaranov/go-openai"

const ProposeActionName = "ProposeAction"

// InternalCapabilityDefinitions are executable registry contracts used to
// derive model-visible tools, but are not themselves advertised.
var InternalCapabilityDefinitions = []CapabilityDefinition{
	{
		ExposedToAgent: false,
		Tool: openai.Tool{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{
			Name: ProposeActionName,
			// Base template only — never shown to the model. The window exposes one
			// Request<Operation> variant per write op (dispatch_window.go), each
			// carrying only that operation's semantic description and field schema.
			// The shared action-first behavior rule lives once in the system prompt
			// (segmentCentralAgentBehavior).
			Description: "写操作候选请求的内部 Schema 模板，不向模型暴露。",
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
}
