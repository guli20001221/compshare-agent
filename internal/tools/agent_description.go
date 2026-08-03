package tools

import "strings"

// WorkflowAgentDescription is the description shown on generated
// Request<Workflow> tools. The registry keeps the operation-specific factual
// boundary; this helper adds one shared interaction contract so the model no
// longer has to infer whether an incomplete proposal should become a prose
// question, a guided card, or an unsafe retry.
//
// It is intentionally applied only to model-visible proposal tools. Internal
// workflow definitions and API tools keep their own terse descriptions.
func WorkflowAgentDescription(operation, operationBoundary string) string {
	boundary := strings.TrimSpace(operationBoundary)
	if boundary == "" {
		boundary = "仅在用户明确要求执行该操作时调用。"
	}
	parts := []string{
		"何时调用：" + boundary,
		"不会做什么：不直接执行、不绕过确认、不猜字段。",
		"卡片如何接续：缺项时引导卡，齐备后确认卡。",
		"失败后下一步：按 needs_input/retry_later/choose_alternative/failed 处理。",
	}
	if example, ok := workflowInputExamples[operation]; ok {
		parts = append(parts, "输入示例（仅填用户已明确字段）："+example)
	}
	return strings.Join(parts, " ")
}

// Complex, multi-field workflows get a deliberately short example. These are
// examples of the proposal-tool input, not a requirement to collect every
// field before calling: the workflow's guided card remains the authority for
// missing selections.
var workflowInputExamples = map[string]string{
	"CreateInstanceWorkflow":    `{"GpuType":"4090","Zone":"cn-wlcb-01"}`,
	"ReinstallInstanceWorkflow": `{"UHostId":"uhost-...","ImageSource":"community"}`,
	"CreateCustomImageWorkflow": `{"UHostId":"uhost-...","Name":"team-base-v1"}`,
	"CloneCustomImageWorkflow":  `{"CompShareImageId":"csi-...","Zone":"cn-wlcb-02"}`,
	"CreateCFSWorkflow":         `{"Name":"training-data","Zone":"cn-wlcb-01"}`,
}
