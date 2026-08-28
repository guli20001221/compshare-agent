package tools

// The in-instance lane has one product contract: when the deployment enables autonomous writes and
// the server can prove the user-selected target, it diagnoses and performs task-scoped guest-local
// repair without confirmation cards. Irrecoverable and tenant/control-plane boundary violations
// remain refused.
const (
	instanceOpsTriggerDesc = "登录指定实例处理平台 API 看不到的 guest 状态：GPU/CUDA、资源与磁盘、环境依赖、进程、服务和端口故障；" +
		"也用于查看当前文件、目录、日志、进程、监听、配置和服务。即使用户只要求只读检查也应调用，并在 Task 写明不修改；不得用公共模型、镜像目录或知识库替代实例内观察。" +
		"用户明确委托安装/升级、下载模型/文件到指定磁盘、改配置、启停/重载服务或运行任务时，直接执行并验证，不要只给用户手工命令或声称无法进入。" +
		"仅对本轮明确指定或同一会话最后一次 user_selected 的目标使用；该绑定不因时间间隔失效，新目标会替换旧目标。目标不唯一时先让用户选择，绝不能从列表自行挑选；OCR、账号唯一实例、被动查询和模型选择不能授权。" +
		"部署开启自主修复后不再额外弹授权卡；"

	instanceOpsWriteDesc = instanceOpsTriggerDesc +
		"自主取证、执行必要修复并验证，不逐命令请求确认；优先可观察、精确、可回滚且只修已证实故障。" +
		"不可恢复删除、格式化、重启/关机、账号密码、关闭 SSH/网络、跨主机写入和控制面动作会被拒绝；说明未解决边界，不提供绕过。" +
		"本轮消息中明确给出 Authorization 时，仅本次交给结构化 HTTP 探针；Task 说明鉴权目标，不要索要、复制或猜测凭据值。"

	instanceOpsNotForDesc = "不用于计费（用 DiagnoseBilling）、平台安全组/端口开放或非具体实例的通用知识。"
)

// InstanceOpsDescription is the single model-facing contract for the lane.
func InstanceOpsDescription() string {
	return instanceOpsWriteDesc + instanceOpsNotForDesc
}
