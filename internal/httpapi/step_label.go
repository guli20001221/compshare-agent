package httpapi

import (
	"strings"

	"github.com/compshare-agent/internal/tools"
	"github.com/compshare-agent/internal/workflow"
)

// stepActionLabels maps a step frame's Action — an internal tool name — to the
// Chinese label the console shows in its activity stream.
//
// Why the server owns this instead of the frontend: the tool names come from
// three unrelated places (tools.Registry, the "ReadCapability_"+intent family
// in internal/capability, and the Request<Operation> proposal tools generated
// from the write catalog). A hand-kept map in the console covered one source at
// a time and missed the others three separate times — each caught only by a
// live run, and the miss was always on the first step of the turn, i.e. the one
// line the user reads first. Sending the label with the frame makes the server,
// which owns the names, also own their presentation; TestStepActionLabelCovers
// fails the build when a new tool arrives without one.
//
// A missing entry is not fatal: stepActionLabel returns "" and the frame simply
// omits Label, leaving the console on its own fallback. Degrade, never break.
var stepActionLabels = map[string]string{
	// --- internal/tools/registry.go: read/query tools -----------------------
	// These are not in the model's window (deterministic handlers call them),
	// but the handler path emits them as steps verbatim — engine.go's
	// planner-handler emit sites pass the raw API action through.
	"DescribeCompShareInstance":               "查询实例信息",
	"DescribeAvailableCompShareInstanceTypes": "查询可用机型",
	"DescribeCompShareImages":                 "查询镜像列表",
	"DescribeCompShareImageTags":              "查询镜像标签",
	"DescribeCompShareCustomImages":           "查询自制镜像",
	"DescribeCompShareSharingImages":          "查询共享镜像",
	"DescribeCommunityImages":                 "查询社区镜像",
	"DescribeCompShareSoftwarePort":           "查询软件端口",
	"DescribeCompShareGpuInventory":           "查询 GPU 库存",
	"DescribeCFS":                             "查询文件存储",
	"DescribeModelRepositoryModels":           "查询模型列表",
	"DescribeModelRepositoryTags":             "查询模型标签",
	"CheckCompShareResourceCapacity":          "查询资源库存",
	"CheckCompShareNetOptimizer":              "检查网络加速",
	"GetCompShareInstanceMonitor":             "查询实例监控",
	"GetCompShareInstancePrice":               "查询实例价格",
	"GetCompShareInstanceUserPrice":           "查询实例价格",
	"GetCompShareInstanceUpgradePrice":        "查询升级价格",
	"GetCompShareRefundPrice":                 "查询退款金额",
	"GetCompShareCFSPrice":                    "查询文件存储价格",
	"GetCompShareCFSUpgradePrice":             "查询文件存储升级价格",
	"GetCompShareCFSRefundPrice":              "查询文件存储退款金额",
	"SearchKnowledge":                         "搜索知识库",
	"ReadChunk":                               "查看知识原文",
	"DiagnoseBilling":                         "诊断扣费异常",
	// Byte-identical to the console's own STEP_LABELS entry, so shipping the
	// server-sent label changes no rendered text — it only stops the console
	// having to keep guessing. The confirmation card says 进入实例只读排查
	// instead; that is a different string for a different frame, not a drift.
	"DiagnoseInstanceInternals": "实例内只读排查",

	// --- internal/capability: the "ReadCapability_"+intent family -----------
	// Second source. These ARE the model's read surface, so in a typical turn
	// the very first step is one of them (ReadCapability_resource_info).
	"ReadCapability_resource_info":              "查询实例信息",
	"ReadCapability_monitor_query":              "查询实例监控",
	"ReadCapability_monitor_history":            "查询历史监控",
	"ReadCapability_gpu_specs_query":            "查询 GPU 规格",
	"ReadCapability_stock_availability":         "查询资源库存",
	"ReadCapability_image_list":                 "查询镜像列表",
	"ReadCapability_image_tag_catalog":          "查询镜像标签",
	"ReadCapability_zone_catalog":               "查询可用区",
	"ReadCapability_model_repository_browse":    "查询模型仓库",
	"ReadCapability_network_accelerator_status": "查询网络加速状态",
	"ReadCapability_pricing_query":              "查询价格",
	"ReadCapability_refund_estimate":            "查询退款金额",
	"ReadCapability_instance_access":            "检查实例接入方式",
	"ReadCapability_account_finance_status":     "查询账户费用",
	"ReadCapability_cfs_list":                   "查询文件存储",
	"ReadCapability_cfs_create_price":           "查询文件存储价格",
	"ReadCapability_cfs_upgrade_price":          "查询文件存储升级价格",
	"ReadCapability_cfs_refund_estimate":        "查询文件存储退款金额",

	// --- internal/tools: standalone consts, outside the Registry list -------
	// The lane's own card says 实例内排查与修复 (it authorizes entering the box). This one authorizes
	// ONE change and is shown with the literal command, so it must not reuse that label — a user who
	// sees the same words twice cannot tell which question they just answered.
	// The same gate now covers literal shell commands and hash-bound structured file/job operations.
	// "操作" is the honest common label; the card summary still shows the exact effect being approved.
	"InstanceOpsWriteCommand": "执行实例内修复操作",

	// --- internal/engine: not a tool at all -------------------------------
	// The deterministic notice a turn emits when the PREVIOUS diagnosis ended without a verdict.
	// Nothing dispatches on this action; it exists so the console can label the frame rather than
	// rendering a bare identifier above the one message that says the box may have been changed.
	"InstanceOpsInterrupted": "上一轮排查中断记录",

	"ProposeAction": "生成操作提案",
}

// stepActionLabel returns the console label for a step Action, or "" when the
// action has none (unknown/ad-hoc actions, which the console renders raw).
func stepActionLabel(action string) string {
	// This is the RUNNING-ACTIVITY line, not the authorization card — an earlier
	// version of this comment claimed it was the card, and that mistake is why the
	// card kept saying 只读 in write mode for a while: fixing this one read as
	// having fixed both. The card is serverOwnedConfirmLabel below.
	if action == "DiagnoseInstanceInternals" && tools.InstanceOpsWritesEnabled() {
		return "实例内排查与修复"
	}
	if label := workflow.StepLabel(action); label != "" {
		return label
	}
	if label, ok := stepActionLabels[action]; ok {
		return label
	}
	// Fourth source: Request<Operation> proposal tools are *generated* per write
	// operation (engine/dispatch_window.go::proposalToolName strips the Workflow
	// suffix), so hand-listing them would go stale the next time a write op is
	// added. Derive from the workflow's own label instead — a new write op then
	// gets its proposal label for free.
	if operation, ok := strings.CutPrefix(action, "Request"); ok && operation != "" {
		if label := workflow.StepLabel(operation + "Workflow"); label != "" {
			return "发起" + label + "请求"
		}
	}
	return ""
}

// serverOwnedConfirmLabel returns the title for an authorization card when the
// SERVER is the only party that can know it, and "" when the console's own map is
// authoritative (every workflow: its wording is fixed and already correct there).
//
// The console keeps a CONFIRM_LABELS map keyed on the action name alone. That is
// fine for a workflow, whose card always says the same thing, and wrong for both
// of the in-instance lane's cards — which is how BOTH shipped mislabelled:
//
//   - DiagnoseInstanceInternals: the card said 进入实例只读排查 while
//     agent.ssh_ops.allow_writes was on. The wording depends on boot state the
//     browser cannot see, so no client-side map can ever get it right. Consent
//     that under-describes what it authorizes is the one defect here that cannot
//     be repaired after the fact.
//   - InstanceOpsWriteCommand: the console had no entry at all, so the per-write
//     card rendered the raw English action name. A card nobody can read is not a
//     gate; observed live on a real repair run.
//
// Deliberately NOT "send a label for every confirmation": that would add a key to
// frames legacy clients receive, which TestConfirmationEvent_LegacyWireShapeUnchanged
// pins on purpose. "" keeps those frames byte-identical via omitempty.
func serverOwnedConfirmLabel(action string) string {
	switch action {
	case "DiagnoseInstanceInternals":
		if tools.InstanceOpsWritesEnabled() {
			return "进入实例排查与修复"
		}
		return "进入实例只读排查"
	case "InstanceOpsWriteCommand":
		// Same string the step stream uses, from the same map, so the card the user
		// approves and the line they then watch scroll cannot drift apart.
		return stepActionLabels["InstanceOpsWriteCommand"]
	}
	return ""
}
