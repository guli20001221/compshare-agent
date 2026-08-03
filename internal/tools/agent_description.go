package tools

import "strings"

// WorkflowAgentDescription is the description shown on generated
// Request<Workflow> tools. The registry remains the source of each operation's
// factual boundary; this helper adds only the interaction deltas the model
// cannot infer from that boundary.
//
// It is intentionally applied only to model-visible proposal tools. Internal
// workflow definitions and API tools keep their own terse descriptions.
func WorkflowAgentDescription(operation, operationBoundary string, guidedIntake bool) string {
	boundary := strings.TrimSpace(operationBoundary)
	if boundary == "" {
		boundary = "仅在用户明确要求执行该操作时调用。"
	}
	cardContinuation := "提交后缺项→按 missing_fields 追问；齐备→确认卡。"
	if guidedIntake {
		cardContinuation = "提交后缺项→引导卡；齐备→确认卡。"
	}
	parts := []string{
		"调用/边界：" + boundary,
		"接续：" + cardContinuation,
		"失败：按根级工具结果处理。",
	}
	if example, ok := workflowInputExamples[operation]; ok {
		parts = append(parts, "输入示例（仅填用户已明确字段）："+example)
	}
	return strings.Join(parts, " ")
}

// Complex, multi-field workflows get a deliberately short example. These are
// examples of the proposal-tool input, not a requirement to collect every
// field before calling: the workflow resolver and confirmation flow remain the
// authority for missing selections.
var workflowInputExamples = map[string]string{
	"CreateInstanceWorkflow":    `{"GpuType":"4090","Zone":"cn-wlcb-01"}`,
	"ReinstallInstanceWorkflow": `{"UHostId":"uhost-...","ImageSource":"community"}`,
	"CreateCustomImageWorkflow": `{"UHostId":"uhost-...","Name":"team-base-v1"}`,
	"CloneCustomImageWorkflow":  `{"CompShareImageId":"csi-...","Zone":"cn-wlcb-02"}`,
	"CreateCFSWorkflow":         `{"Name":"training-data","Zone":"cn-wlcb-01"}`,
}
