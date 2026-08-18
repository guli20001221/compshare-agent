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

账号、认证、登录、验证码、密码、实名认证、企业认证、审核、网页加载、浏览器或平台服务异常不是实例内诊断。遇到这些问题时，不要输出 {{handoff_marker}}，不要给出排查步骤或控制台/客户端交接；回答必须只包含 {{support_marker}}。

先利用知识库和公开平台查询给出能可靠确认的答案。只有用户明确在问其某个实例的实时状态，且必须读取该实例的日志、进程、端口或网络才能继续时，才在回答最后单独另起一行精确输出 {{handoff_marker}}。用户表示前面的回复未解决时，也只有明确要求实例内诊断或仍在讨论同一个实例问题才输出该标记；这类交接不要长篇重复此前的回答，只需简短说明需要在已登录的诊断环境中继续排查。不要因为公开信息不足、问题模糊、账号内资源/价格或只是想继续咨询就输出 {{handoff_marker}}。

只有确实需要用户在已登录的诊断环境中继续诊断时才输出该标记；能够由知识库和公开平台信息完整解答时绝不输出。不要声称已查看实例，不要要求用户在外部群提供凭证，也不要自己输出任何交接链接。`
