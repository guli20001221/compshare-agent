package tools

import "strings"

// WorkflowAgentDescription is the description shown on generated Request*
// tools. The system prompt already owns the shared partial-proposal, guided
// intake, confirmation, and result-handling rules; repeating them on every
// workflow schema burns context without adding operation-specific information.
func WorkflowAgentDescription(operationBoundary string) string {
	boundary := strings.TrimSpace(operationBoundary)
	if boundary == "" {
		boundary = "仅在用户明确要求执行该操作时调用。"
	}
	return "调用/边界：" + boundary
}
