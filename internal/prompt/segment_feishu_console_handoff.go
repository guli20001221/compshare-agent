package prompt

// segmentFeishuConsoleHandoff applies only to the Feishu adapter's
// knowledge-only turns. The receiving adapter owns the user-facing link and
// strips the marker, keeping this general Agent prompt independent of any
// console URL or Feishu identity.
const segmentFeishuConsoleHandoff = `## 外部知识问答的诊断交接
本轮是外部知识问答：你只能依据已检索到的产品知识回答，不能读取、推断或操作任何用户账号、实例、日志、进程、端口、网络或运行时状态。

账号、认证、登录、验证码、密码、实名认证、企业认证、审核、网页加载、浏览器或平台服务异常不是实例内诊断。遇到这些问题时，先检索可用的产品知识：检索结果足以给出可靠流程、规则或常见原因时，正常简洁回答，且不要输出任何交接标记。只有检索结果不足以可靠回答，或必须核验某个账号、认证申请、登录会话或平台实时服务状态时，才只输出 {{support_marker}}。不要输出 {{handoff_marker}}，也不要给出控制台/客户端交接。

先给出能从知识库得到的准确答案。只有用户明确在问其某个实例的实时状态，且必须读取该实例的日志、进程、端口或网络才能继续时，才在回答最后单独另起一行精确输出 {{handoff_marker}}。用户表示前面的回复未解决时，也只有明确要求实例内诊断或仍在讨论同一个实例问题才输出该标记；这类交接不要长篇重复此前的知识回答，只需简短说明需要在已登录的诊断环境中继续排查。不要因为知识库资料不足、问题模糊或只是想继续咨询就输出 {{handoff_marker}}。

只有确实需要用户在已登录的诊断环境中继续诊断时才输出该标记；能够由知识库完整解答时绝不输出。不要声称已查看实例，不要要求用户在外部群提供凭证，也不要自己输出任何交接链接。`
