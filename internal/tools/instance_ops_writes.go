package tools

// The in-instance lane has one compact product contract. Mode separates a genuine inspection from
// autonomous repair at the typed runtime boundary; relying on Task prose alone would let an outer
// planner accidentally grant writes after the user explicitly asked for no changes.
const instanceOpsWriteDesc = "登录指定实例，读取或处理平台 API 看不到的 Guest 状态，并实测诊断服务到 SSH 入口：" +
	"文件/目录/日志、进程/服务/监听、GPU/CUDA、资源/磁盘和环境依赖。特定实例的这些实时问题必须调用，不得以公共模型/镜像目录或知识库代替。" +
	"Mode=inspect 用于只读或用户明确禁止修改；Mode=repair 用于用户委托安装/升级、下载、改配置、启停/重载服务或运行任务，可自主执行并验证任务内可恢复修复，不只给手工命令。" +
	"仅使用本轮明确指定或会话最后一次 user_selected 的目标（不因时间失效）；新目标替换旧目标，目标不唯一先询问，不从列表自选。OCR、账号唯一实例、被动查询和模型选择均不授权。" +
	"不弹实例内或逐命令确认。不可恢复删除、格式化、重启/关机、账号密码、关闭 SSH/网络、跨主机或控制面写入会被拒绝；说明边界，不给绕过。" +
	"Authorization 仅在本轮交给结构化 HTTP 探针，Task 只写鉴权目标，不复制或猜值。不用于计费、平台侧端口变更或无具体实例的通用知识。"

// InstanceOpsDescription is the single model-facing contract for the lane.
func InstanceOpsDescription() string {
	return instanceOpsWriteDesc
}
