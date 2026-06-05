package prompt

import (
	"strings"

	"github.com/compshare-agent/internal/diagnosis"
	"github.com/compshare-agent/internal/intent"
	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
)

func renderWorkflowSelectionCard() string {
	return renderActionCatalog(workflow.RegisteredWorkflowActions())
}

func renderDiagnosisSelectionCard() string {
	return renderActionCatalog(diagnosis.RegisteredDiagnosisActions())
}

func renderWorkflowActionNameList() string {
	return strings.Join(workflow.RegisteredWorkflowActions(), " / ")
}

func renderActionCatalog(actions []string) string {
	descriptions := toolDescriptionsByName()
	var b strings.Builder
	for i, action := range actions {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("  - ")
		b.WriteString(action)
		b.WriteString("：")
		b.WriteString(descriptions[action])
	}
	return b.String()
}

func toolDescriptionsByName() map[string]string {
	descriptions := make(map[string]string, len(tools.Registry))
	for _, tool := range tools.Registry {
		if tool.Function == nil {
			continue
		}
		descriptions[tool.Function.Name] = tool.Function.Description
	}
	return descriptions
}

// RenderIntentScopedReActCard returns the per-turn ReAct guidance card for an
// already classified planner intent. Empty means no extra card is needed.
func RenderIntentScopedReActCard(intentName intent.Intent, mutatingToolsEnabled bool) string {
	switch intentName {
	case intent.IntentOperationLifecycle:
		if !mutatingToolsEnabled {
			return renderReadOnlyOperationBoundaryCard()
		}
		return strings.Join([]string{
			"## 本轮 ReAct 操作卡片",
			"工作流目录：",
			renderWorkflowSelectionCard(),
			"补充边界：",
			"- 创建实例请求必须使用 CreateInstanceWorkflow，不要直接调 CreateCompShareInstance。",
			`- 用户提到 PyTorch/CUDA/vLLM 等框架环境时，平台镜像优先，带上 ImageName（如 ImageName="PyTorch"）。`,
			"- 用户提到 Ubuntu/Windows/裸系统/干净环境时，使用平台镜像，不传 ImageName 即可。",
			`- 用户提到具体应用名（ComfyUI、SD WebUI、Stable Diffusion、Dify、Ollama 等）时，传 ImageSource="community" + ImageName="应用名"，使用社区镜像创建。`,
			"- 创建失败（如售罄）后不要自动重试其他 GPU，应将失败原因告知用户，让用户决定下一步。",
			"- 推荐替代 GPU 前，必须先用 CheckCompShareResourceCapacity 确认有库存，不要推荐后再发现没货。",
			"- CreateDiskWorkflow 必须带 Size（GB）；用户没说容量时先追问，不要进入确认；这是新建盘，不是扩已有盘。",
			"- ResizeDiskWorkflow 的 Size 是目标容量不是新增容量；扩系统盘传 DiskType=Boot；多块数据盘中扩某一块必须传 DiskId。",
			"- CreateCustomImageWorkflow 用户未提供镜像 Name 时必须先追问名称；不要直接调用 raw CreateCompShareCustomImage，不要发布社区镜像。",
			"- 调用 *Workflow 工具（" + renderWorkflowActionNameList() + "）时，禁止在工具调用前生成任何文本内容；Workflow 卡片是用户确认参数和风险的唯一入口。",
			"- 涉及实例变更前必须重新调用 DescribeCompShareInstance 获取最新状态，不要只凭历史对话中的状态。",
		}, "\n")
	case intent.IntentDiagnosis, intent.IntentVagueFailure:
		return strings.Join([]string{
			"## 本轮 ReAct 诊断卡片",
			"诊断目录：",
			renderDiagnosisSelectionCard(),
			"补充边界：",
			"- 模糊故障先追问哪台实例和具体现象，不得直接调用任何 Diagnose* 工具。",
			"- " + sharedInstanceReadOnlySelfCheckCommandRule,
			"- " + sharedOptionalRepairCommandRule,
			"- 对诊断续问必须重新查询当前事实，不要直接复用上一轮诊断结论。",
		}, "\n")
	case intent.IntentResourceInfo, intent.IntentMonitorQuery, intent.IntentBillingInstance, intent.IntentDiskInfo:
		return strings.Join([]string{
			"## 本轮 ReAct 查询卡片",
			"- 查询当前状态、价格、监控、库存、镜像和实例详情时必须基于工具返回事实。",
			"- " + sharedCompleteListingRule,
		}, "\n")
	case intent.IntentRecommendation:
		return strings.Join([]string{
			"## 本轮 ReAct 推荐卡片",
			"- 选型建议必须先调用规格、库存或价格工具获取事实。",
			"- 推荐替代 GPU 前，必须先用 CheckCompShareResourceCapacity 确认可用库存。",
		}, "\n")
	case intent.IntentKnowledgeQA:
		return strings.Join([]string{
			"## 本轮 ReAct 知识卡片",
			"- 平台知识类问题必须通过知识库/RAG资料、工具事实或诊断结果回答。",
			"- 当前轮次没有资料或事实时，不要凭模型记忆直接回答。",
		}, "\n")
	case intent.IntentUnknown:
		return strings.Join([]string{
			"## 本轮 ReAct 范围卡片",
			"- 只处理优云算力共享平台相关问题。",
			"- 与平台无关的请求必须按范围边界拒答。",
		}, "\n")
	default:
		return ""
	}
}

func renderReadOnlyOperationBoundaryCard() string {
	return strings.Join([]string{
		"## 本轮 ReAct 只读操作卡片",
		"- 当前阶段不直接执行开机、关机、重启、重置密码、创建实例、改名、定时关机等变更操作。",
		"- 用户提出变更操作时，可以提供控制台操作步骤和注意事项，但不要声称已经替用户执行。",
	}, "\n")
}
