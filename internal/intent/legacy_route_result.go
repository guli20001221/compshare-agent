package intent

import "github.com/compshare-agent/internal/llm"

// OutputMode and IntentRouterResult remain only as the result envelope consumed
// by the already-unreachable direct-dispatch compatibility code. The model
// router that produced them is gone; R2.2 removes the remaining consumers.
type OutputMode string

const (
	OutputModeJSONSchema       OutputMode = "json_schema"
	OutputModeJSONObject       OutputMode = "json_object"
	OutputModeStrictPromptJSON OutputMode = "strict_prompt_json"
)

type IntentRouterResult struct {
	Plan                IntentRoute
	Mode                OutputMode
	Attempts            int
	Fallback            bool
	LastValidationCode  ErrorCode
	LastValidationField string
	LastRejectedIntent  Intent
	Usage               llm.TokenUsage
}

func unknownFallbackPlan() IntentRoute {
	return IntentRoute{SchemaVersion: SchemaVersion, Intent: IntentUnknown}
}
