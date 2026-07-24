package httpapi

import "strings"

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

	// --- internal/tools/registry.go: mutating workflows ---------------------
	// Reachable as steps even in the P6 proposal world: an unaccepted workflow
	// call is emitted as a StepBlocked with the workflow name (engine.go's
	// workflow.IsWorkflowTool branch). They also seed the Request* labels below.
	"CreateInstanceWorkflow":      "创建实例",
	"StopInstanceWorkflow":        "关闭实例",
	"StartInstanceWorkflow":       "启动实例",
	"RebootInstanceWorkflow":      "重启实例",
	"RenameInstanceWorkflow":      "重命名实例",
	"ResetPasswordWorkflow":       "重置密码",
	"ResizeInstanceWorkflow":      "调整配置",
	"ReinstallInstanceWorkflow":   "重装系统",
	"SetStopSchedulerWorkflow":    "设置定时关机",
	"CancelStopSchedulerWorkflow": "取消定时关机",
	"CreateDiskWorkflow":          "创建数据盘",
	"ResizeDiskWorkflow":          "扩容数据盘",
	"CreateCustomImageWorkflow":   "创建自制镜像",
	"CloneCustomImageWorkflow":    "复制自制镜像",
	"EnableNetOptimizerWorkflow":  "开启网络加速",
	"CreateCFSWorkflow":           "创建文件存储",
	"ResizeCFSWorkflow":           "扩容文件存储",

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
	// Third source (read_platform_capability.go). UpdateTaskState is the first
	// tool the model reaches for on a multi-step turn, so an unlabelled entry
	// here is maximally visible.
	"UpdateTaskState": "整理任务状态",
	"ProposeAction":   "生成操作提案",
}

// stepActionLabel returns the console label for a step Action, or "" when the
// action has none (unknown/ad-hoc actions, which the console renders raw).
func stepActionLabel(action string) string {
	if label, ok := stepActionLabels[action]; ok {
		return label
	}
	// Fourth source: Request<Operation> proposal tools are *generated* per write
	// operation (engine/dispatch_window.go::proposalToolName strips the Workflow
	// suffix), so hand-listing them would go stale the next time a write op is
	// added. Derive from the workflow's own label instead — a new write op then
	// gets its proposal label for free.
	if operation, ok := strings.CutPrefix(action, "Request"); ok && operation != "" {
		if label, ok := stepActionLabels[operation+"Workflow"]; ok {
			return "发起" + label + "请求"
		}
	}
	return ""
}
