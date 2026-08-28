package tools

// The in-instance lane has one product contract: when the deployment enables autonomous writes and
// the server can prove the user-selected target, it diagnoses and performs task-scoped guest-local
// repair without confirmation cards. Irrecoverable and tenant/control-plane boundary violations
// remain refused.
const (
	instanceOpsTriggerDesc = "登录到指定实例内部排查问题或完成用户明确委托的 guest 本地操作。适用于根因在实例内部、平台 API 看不到的故障：" +
		"GPU 掉卡 / nvidia-smi 报错 / CUDA 找不到设备、显存被占满、服务或端口起不来（ComfyUI、Jupyter、vLLM 等）、" +
		"磁盘写满、数据盘未挂载、Python 环境与依赖异常、进程卡死或负载异常，也可验证平台诊断服务到实例 SSH 入口的实际连接结果。" +
		"也用于用户明确委托的实例内安装或升级、下载模型/文件到指定磁盘、修改配置、启停或重载服务、运行或恢复任务；" +
		"应直接执行并验证，不要只给用户手工命令，也不要错误声称无法进入实例。" +
		"仅在用户已明确指定实例或当前选择已唯一绑定时使用；多个实例且未指定目标时先让用户选择，绝不能从列表自行挑选。" +
		"只有当前消息明确指定，或同一会话里最后一次由用户明确选定的 user_selected 目标才可直接进入；该绑定不因时间间隔失效，用户明确指定新目标时才切换。" +
		"不能用 OCR、账号唯一实例、被动查询结果或模型自行选择来补足目标。" +
		"部署已开启实例内自主修复时，不再额外弹授权卡；"

	instanceOpsWriteDesc = instanceOpsTriggerDesc +
		"应自主收集证据、执行必要修复并验证结果，不为每条命令重复请求确认；" +
		"优先使用可观察、精确且可回滚的动作，只修已证实的故障。不可恢复的数据删除、格式化磁盘、重启/关机、改密码或账号、" +
		"关闭 SSH/网络、跨主机写入和控制面动作始终会被拒绝；请说明边界与未解决事项，不要提供等价绕过命令。" +
		"若用户在本轮消息中明确给出 Authorization 请求头，系统会把它作为仅本次诊断有效的私有能力交给实例内结构化 HTTP 探针；" +
		"任务只需说明要验证的鉴权目标，不要索要、复制或猜测凭据值。"

	instanceOpsNotForDesc = "不用于：费用与计费（用 DiagnoseBilling）、" +
		"平台侧安全组与端口开放（不在实例内）、不针对具体实例的通用知识问题。"
)

// InstanceOpsDescription is the single model-facing contract for the lane.
func InstanceOpsDescription() string {
	return instanceOpsWriteDesc + instanceOpsNotForDesc
}
