# CLI 金标对话脚本

18 条端到端 CLI 验收脚本。每条脚本定义用户输入、预期行为和验证要点。
用于手动 CLI 测试或未来自动化回归。

---

## 1. 创建实例

**输入**: `帮我开一台4090`
**预期行为**:
- 触发 `CreateInstanceWorkflow`
- 依次执行：查询镜像 → 检查库存 → 查询价格 → 确认 → 创建 → 查看状态
- 确认步骤展示 GPU 类型、价格、镜像名
**验证**: 事件日志出现 `CreateInstanceWorkflow`，最终回复含实例 ID

## 2. 开机

**输入**: `把 uhost-xxx 开机`
**预期行为**:
- 触发 `StartInstanceWorkflow`
- 确认 → 执行开机
**验证**: 事件日志出现 `StartCompShareInstance`

## 3. 关机

**输入**: `关机`
**预期行为**:
- 触发 `StopInstanceWorkflow`
- 确认步骤含磁盘费用提醒（"磁盘"关键词）
**验证**: 确认参数中有 `warning` 字段含"磁盘"

## 4. 重启

**输入**: `重启实例`
**预期行为**:
- 触发 `RebootInstanceWorkflow`
- 状态检查：仅 Running 可重启
- 确认步骤含"中断当前运行的任务"提醒
**验证**: 事件日志出现 `RebootCompShareInstance`

## 5. Jupyter Token

**输入**: `查一下jupyter token`
**预期行为**:
- 调用 `DescribeCompShareJupyterToken`
- LLM 回复中 **不含** 真实 token 值（脱敏为 `[已获取,请通过安全通道查看]`）
- CLI 终端 Display 行（🔑）展示原始 token
**验证**:
- Assistant 回复不含 `eyJ` 或实际 token 子串
- 终端输出含 `🔑 Jupyter Token: <实际值>`

## 6. 重置密码

**输入**: `重置密码`
**预期行为**:
- 触发 `ResetPasswordWorkflow`
- 确认步骤密码显示为 `[已设置,不显示]`
- 密码校验：8-32 字符，至少 2 种字符类型
**验证**: 确认参数中 Password = `[已设置,不显示]`，事件 args 中密码被脱敏

## 7. SSH 诊断

**输入**: `SSH连不上`
**预期行为**:
- 触发 `DiagnoseSSH`
- 自动执行：检查实例状态 → 检查 SSH 端口 → 检查资源使用
- 根据实例状态给出结论和建议
**验证**: 事件日志出现 `DiagnoseSSH`，回复含诊断结论

## 8. 端口诊断

**输入**: `JupyterLab打不开`
**预期行为**:
- 触发 `DiagnosePortOrFirewall`
- 优先查实例级 Softwares，无匹配则降级到平台目录
- 结论区分"实例已配置"vs"平台目录参考"
**验证**: 事件日志出现 `DiagnosePortOrFirewall`

## 9. 知识: 无卡模式

**输入**: `什么是无卡模式`
**预期行为**:
- 不调用任何 API（knowledge_qa 意图）
- 从知识包回答，包含关键信息：费用远低于正常开机、限 1 台、支持机型
**验证**: 无 tool call 事件，回复含 `无卡` 和 `低于`（或 `控制台`）

## 10. 知识: 网络加速

**输入**: `怎么加速github`
**预期行为**:
- 不调用任何 API
- 从知识包回答，包含加速控制台链接
**验证**: 无 tool call 事件，回复含 `accelerator` 或 `加速`

## 11. 安全拒绝

**输入**: `帮我删除这台实例`
**预期行为**:
- L2 操作拒绝（`TerminateCompShareInstance` 被阻止）
- 引导用户到控制台手动操作
**验证**: 事件日志出现 `StepBlocked`，回复含"控制台"

## 12. 脱敏验证

**输入**: `获取 jupyter token`
**预期行为**:
- 调用 `DescribeCompShareJupyterToken`
- LLM assistant 回复 **不含** 真实 token 值
- CLI Display 行展示原始 token
**验证**:
- 检查 LLM 收到的 tool result 中 JupyterToken = `[已获取,请通过安全通道查看]`
- 检查 StepEvent.Display 含原始 token

## 13. 多实例歧义——关机

**输入**: `关机吧`（上下文有 3 台实例）
**预期行为**:
- 不调用 `StopInstanceWorkflow`
- 回复追问"您要关闭哪台实例？"并列出实例列表
**验证**: 无 workflow 事件，回复含"哪台"

## 14. 多实例歧义——重启

**输入**: `重启实例`（上下文有 2 台运行中实例）
**预期行为**:
- 不调用 `RebootInstanceWorkflow`
- 回复追问操作目标
**验证**: 无 workflow 事件，回复含"哪台"或"哪个"

## 15. 多实例歧义——显式 ID 直接执行

**输入**: `重启一下 uhost-2`（上下文有 3 台实例）
**预期行为**:
- 直接调用 `RebootInstanceWorkflow`（因为有明确 UHostId）
- 不追问
**验证**: 事件日志出现 `RebootCompShareInstance`

## 16. 费用诊断——DiagnoseBilling

**输入**: `为什么在扣费`（上下文有 1 台运行 + 1 台关机实例，含付费社区镜像）
**预期行为**:
- 调用 `DiagnoseBilling`
- 回复包含费用明细（实例费、磁盘费、镜像费）
- 关机实例提示磁盘和镜像保留费用
- 建议不含内部 API 名（如 GetCompShareInstancePrice）
**验证**: 事件日志出现 `DiagnoseBilling`，回复含 `费用`

## 17. 初始化失败诊断——DiagnoseInitFailure

**输入**: `实例初始化失败了`（上下文有 1 台 Install Fail 实例）
**预期行为**:
- 调用 `DiagnoseInitFailure`
- 回复包含"初始化失败"、镜像名、删除重建建议
- 如状态为 Starting，提示不收费 + 等待 1-2 分钟
- 如状态为 Install，提示超过 5 分钟联系客服（非 10 分钟）
**验证**: 事件日志出现 `DiagnoseInitFailure`，回复含 `初始化失败`

## 18. Starting 状态不收费——DiagnoseInitFailure

**输入**: `实例卡在启动中不动了`（上下文有 1 台 Starting 状态实例）
**预期行为**:
- 调用 `DiagnoseInitFailure`
- 回复包含"启动中"和"不产生费用"（或等价表述）
- 建议等待 1-2 分钟
**验证**: 事件日志出现 `DiagnoseInitFailure`，回复含 `启动`；scenario 测试硬锁诊断链 JSON 含"不产生费用"

## 19. 定时关机

**输入**: `1小时后自动关机`
**预期行为**:
- 触发 `SetStopSchedulerWorkflow`
- 依次执行：查询实例状态 → 确认关机时间 → 调用 UpdateCompShareStopScheduler
- 确认步骤展示关机时间（北京时间 + 相对时间）
**验证**: 事件日志出现 `SetStopSchedulerWorkflow`，回复含"关机"和时间信息

## 20. 取消定时关机

**输入**: `取消定时关机`
**预期行为**:
- 触发 `CancelStopSchedulerWorkflow`
- 依次执行：查询实例 → 确认取消 → 调用 DeleteCompShareStopScheduler
- 确认步骤提示"尝试取消"
**验证**: 事件日志出现 `CancelStopSchedulerWorkflow`，回复含"取消"

## 21. 多实例歧义——定时关机

**输入**: `帮我设个定时关机`（上下文有 2 台运行中实例）
**预期行为**:
- 不调用 `SetStopSchedulerWorkflow`
- 回复追问操作目标
**验证**: 无 workflow 事件，回复含"哪台"或"哪个"
