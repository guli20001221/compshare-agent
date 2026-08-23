package workflow

// registration is the one catalog for a workflow's executable definition and
// its user-facing names. Keeping the factory, activity label, and reply label
// together prevents the engine and HTTP stream from drifting when a workflow is
// added.
type registration struct {
	action     string
	stepLabel  string
	replyLabel string
	factory    func() *Definition
}

var workflowRegistrations = []registration{
	{"CreateInstanceWorkflow", "创建实例", "创建实例", CreateInstanceDef},
	{"StopInstanceWorkflow", "关闭实例", "关机", StopInstanceDef},
	{"StartInstanceWorkflow", "启动实例", "开机", StartInstanceDef},
	{"RebootInstanceWorkflow", "重启实例", "重启", RebootInstanceDef},
	{"RenameInstanceWorkflow", "重命名实例", "重命名", RenameInstanceDef},
	{"UpdateInstancePortsWorkflow", "更新平台端口", "更新端口", UpdateInstancePortsDef},
	{"ResetPasswordWorkflow", "重置密码", "重置密码", ResetPasswordDef},
	{"SetStopSchedulerWorkflow", "设置定时关机", "设置定时关机", SetStopSchedulerDef},
	{"CancelStopSchedulerWorkflow", "取消定时关机", "取消定时关机", CancelStopSchedulerDef},
	{"ResizeInstanceWorkflow", "调整配置", "变配", ResizeInstanceDef},
	{"ResizeDiskWorkflow", "扩容数据盘", "扩已有盘", ResizeDiskDef},
	{"ReinstallInstanceWorkflow", "重装系统", "重装系统", ReinstallInstanceDef},
	{"CreateDiskWorkflow", "创建数据盘", "创建数据盘", CreateDiskDef},
	{"CreateCustomImageWorkflow", "创建自制镜像", "创建自制镜像", CreateCustomImageDef},
	{"CloneCustomImageWorkflow", "复制自制镜像", "克隆自制镜像", CloneCustomImageDef},
	{"EnableNetOptimizerWorkflow", "开启网络加速", "开启网络加速", EnableNetOptimizerDef},
	{"CreateCFSWorkflow", "创建文件存储", "创建文件存储", CreateCFSDef},
	{"ResizeCFSWorkflow", "扩容文件存储", "扩容文件存储", ResizeCFSDef},
}

func registeredWorkflow(action string) (registration, bool) {
	for _, item := range workflowRegistrations {
		if item.action == action {
			return item, true
		}
	}
	return registration{}, false
}

// RegisteredWorkflowActions returns workflow action names in prompt-stable
// human order.
func RegisteredWorkflowActions() []string {
	actions := make([]string, 0, len(workflowRegistrations))
	for _, item := range workflowRegistrations {
		actions = append(actions, item.action)
	}
	return actions
}

// IsWorkflowTool reports whether the given action name corresponds to a
// registered workflow.
func IsWorkflowTool(action string) bool {
	_, ok := registeredWorkflow(action)
	return ok
}

// GetWorkflow returns a fresh Definition for the named workflow. The second
// return value is false if no workflow is registered under that name.
func GetWorkflow(action string) (*Definition, bool) {
	item, ok := registeredWorkflow(action)
	if !ok {
		return nil, false
	}
	return item.factory(), true
}

// StepLabel is the activity-stream label for a workflow, or "" for an unknown
// action. It intentionally differs from ReplyLabel for a few terse natural
// language replies (for example, "关机" versus "关闭实例").
func StepLabel(action string) string {
	item, ok := registeredWorkflow(action)
	if !ok {
		return ""
	}
	return item.stepLabel
}

// ReplyLabel is the short natural-language name used in engine replies.
func ReplyLabel(action string) string {
	item, ok := registeredWorkflow(action)
	if !ok {
		return ""
	}
	return item.replyLabel
}
