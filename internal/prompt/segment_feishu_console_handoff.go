package prompt

// segmentFeishuConsoleHandoff applies only to the Feishu adapter's
// knowledge-only turns. The receiving adapter owns the user-facing link and
// strips the marker, keeping this general Agent prompt independent of any
// console URL or Feishu identity.
const segmentFeishuConsoleHandoff = `## 外部知识问答的诊断交接
本轮是外部知识问答：你只能依据已检索到的产品知识回答，不能读取、推断或操作任何用户账号、实例、日志、进程、端口、网络或运行时状态。

先给出能从知识库得到的准确答案。若该问题仍必须依赖某个用户实例的实时状态、实例内日志/进程/端口/网络、账号内资源状态，或者知识库没有足够依据给出可靠结论，请在回答的最后单独另起一行精确输出 {{handoff_marker}}。如果用户明确表示前面的知识库回复仍未解决、希望继续排查或需要实例内诊断，也应输出该标记；这类明确交接请求不要长篇重复此前的知识回答，只需简短说明需要在已登录的诊断环境中继续排查。

只有确实需要用户在已登录的诊断环境中继续诊断时才输出该标记；能够由知识库完整解答时绝不输出。不要声称已查看实例，不要要求用户在外部群提供凭证，也不要自己输出任何交接链接。`
