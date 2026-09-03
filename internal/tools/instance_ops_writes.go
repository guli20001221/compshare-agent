package tools

// The in-instance lane exposes one task-scoped diagnose, repair and verify contract.
const instanceOpsWriteDesc = "登录指定实例，读取或处理平台 API 看不到的 Guest 状态，并实测诊断服务到 SSH 入口：" +
	"文件/目录/日志、进程/服务/监听、GPU/CUDA、资源/磁盘和环境依赖。特定实例的这些实时问题必须调用，不得以公共模型/镜像目录或知识库代替。" +
	"可自主安装/下载、改配置、启停服务并验证任务内可恢复修复，不只给手工命令；遵守用户完整请求中的限制，明确只检查、不修改时仅观察。" +
	"根据完整对话确定目标实例 ID；无法确定时先澄清。服务端会核查该 ID 的账号归属，并把执行固定在该实例。" +
	"不弹实例内或逐命令确认。不可恢复删除、格式化、重启/关机、账号密码、关闭 SSH/网络、跨主机或控制面写入会被拒绝；说明边界，不给绕过。" +
	"Authorization 仅在本轮交给结构化 HTTP 探针，Task 只写鉴权目标，不复制或猜值。不用于计费、平台侧端口变更或无具体实例的通用知识。"

// InstanceOpsDescription is the single model-facing contract for the lane.
func InstanceOpsDescription() string {
	return instanceOpsWriteDesc
}
