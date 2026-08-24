package prompt

// segmentFeishuPublicPlatformScope applies whenever the Feishu adapter has
// opted into its fail-closed public platform query window, even when the
// optional console-handoff behavior is disabled.
const segmentFeishuPublicPlatformScope = `## 外部平台查询范围
本轮是外部群问答：除已检索到的产品知识外，你可以查询公开的实时平台信息，包括 GPU 规格、库存、平台/社区镜像目录、可用区、公共模型仓库和目录价。你不能读取、推断或操作任何用户账号、实例、日志、进程、端口、网络、运行时状态、账号价格、自制/共享镜像或其他私有资源。`

// segmentFeishuPublicPlatformConsoleHandoff adds the response-only diagnostic
// handoff contract to the public platform scope. The receiving adapter owns
// URLs and strips the marker before rendering.
const segmentFeishuPublicPlatformConsoleHandoff = segmentFeishuPublicPlatformScope + `

先用知识库和公开平台能力回答能可靠确认的部分。账号、认证、登录或平台服务问题只有在证据不足或必须由平台人员核验时，才调用 HandoffToCustomerSupport。用户明确要求排查某个实例，且必须查看其日志、进程、端口或网络时，在简短回答末尾另起一行输出 {{handoff_marker}}。

不要因问题模糊、公开信息不足、账号内资源或用户想继续咨询就交接；不要声称已查看私有状态、索取凭证或自行输出交接链接。`
