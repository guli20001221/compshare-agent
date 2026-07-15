# Agent 上下文收敛后续执行计划

> 状态：执行中
>
> 当前基线：`origin/main@7b1b9aa5`（PR #455–#464 已合并）
>
> 设计合同：`docs/plans/2026-07-15-agent-runtime-convergence-spec.md`
>
> 本文只描述剩余执行，不重开已经完成的阶段。

## 1. 最终目标

本轮收口不是继续修某几个“失忆句式”，而是保证所有普通业务轮次都遵循同一条规则：

1. 主 Agent 先收到由已提交对话、结构化任务、可信实体、待确认动作、工具观察和已验证知识组成的 `AgentContext`；
2. 主 Agent 决定直接回答、查询、RAG、澄清或提出写操作；
3. 查询和 RAG 结果回到同一个 Agent 循环，写操作进入确定性的 Resolver、Gate 和 Workflow；
4. 回答与状态保存成功后才向前端宣布完成；
5. 除协议、安全和确认外，任何业务规则都不能在主 Agent 看到上下文之前终止本轮。

Agent 可以在已有上下文足以回答时不调用 RAG。需要检索时，检索问题由 Agent 结合完整上下文组织；是否包含“这个”“呢”“多少钱”等词不参与判断。

## 2. 已完成且不得返工的部分

| 阶段 | PR | 已交付能力 |
|---|---:|---|
| P0 | #455 | 架构边界、遗留清单和禁止新增语义补丁的门禁 |
| P1 | #456 | 统一 AgentRuntime 生命周期 |
| P2 | #457 | AgentContext 与 Context Compiler |
| P3 | #458 | Capability Registry |
| P4 | #459 | 只读平台能力适配为 Agent capability |
| P5 | #460 | ActionProposal 与 Resolver 影子链 |
| P6a | #461 | 中心只读运行时切换接缝 |
| P6b | #462 | 写操作经 Proposal、Resolver、确认、journal 与结构化任务状态 |
| P8 | #463 | 统一 Response Gateway 与证据出口 |
| P7a | #464 | 服务端默认进入中心 AgentRuntime，中心 Prompt 成为生产 Prompt |

这些 PR 已经解决中心运行时所需的正向能力。后续只做删除、强制唯一入口和真实验收，不再建设第三套 Router、上下文包、槽位解释器或回答器。

## 3. 当前剩余风险

当前生产服务已选择中心运行时，但仓库仍保留以下退化入口：

- 部分生产构造器和配置仍允许创建旧语义运行时；
- CLI 仍可能构造旧 Router/Planner，旧环境变量和配置仍给人“可以切回”的错觉；
- 独立 Router、ContextDecision、直接业务终点和旧补参代码仍参与编译，大量历史测试把旧行为当合同；
- 活跃文档仍描述 direct dispatch 和 intent-router 开关；
- 还没有用同一组真实上下文记录完成 WebSocket、前端、跨副本、重启和数据库故障的总验收。

因此，`main@7b1b9aa5` 可以证明新主路存在，尚不能宣布“旧路不可能再次运行”。

## 4. 剩余实施阶段

### R1 / P7b：封死生产退化入口

目标：任何正式构造方式都只能得到中心 AgentRuntime。

实施内容：

- `agentpool`、HTTP session、CLI 和持久会话恢复统一强制中心运行时；
- 删除生产 Options 中的中心运行时布尔开关，不允许调用方选择旧路；
- CLI 不再创建 IntentPlanner、Router shadow 或独立 ContextDecision；
- 删除运行时对以下旧开关的读取：
  - `COMPSHARE_DIRECT_DISPATCH_INTENTS`
  - `COMPSHARE_INTENT_ROUTER_MODE`
  - `COMPSHARE_INTENT_ROUTER_STRUCTURED_OUTPUT`
- 删除活动配置和运行文档中的旧开关；历史报告保留，但必须标注为历史行为；
- 删除只验证这些开关的测试，不在测试文件里复制一套旧生产映射；
- 保留底层权限、确认、Workflow 和证据测试，不以“测试难迁移”为理由删除安全合同。

验收门：

- 从 server、CLI、缓存冷建和 durable rehydrate 四个入口创建的 Engine 都使用中心运行时；
- 对构造器做反向变异，任一入口改回旧路时测试必须失败；
- shipping binary 中不再读取上述三个环境变量；
- 全量测试、`go vet` 和架构扫描通过。

退出产物：一个独立 PR。当前 `codex/agent-runtime-p7b` 工作区属于本阶段，完成前不夹带 R2 删除。

### R2 / P7c：删除旧语义系统

目标：旧路不仅“默认不走”，而是无法在生产代码中被重新启用。

R2 按责任拆成三个可审查 PR，但必须连续完成，不保留无限期兼容层。

#### R2.1 删除旧模型决策器

- 删除独立 Router/IntentPlanner 的生产接口、Prompt、结果结构和 Engine 调用点；
- 删除 `llmContextDecisionLayer` 及其 Prompt、缓存和状态修改职责；
- 将仍有价值的测试改写为 AgentContext、AgentStep 和 State Reducer 的行为合同；
- 删除只证明旧 Router 分类结果的历史测试。

门禁：普通业务轮次只出现一次中心模型循环；trace 中不存在第二个语义模型身份；低置信、模型格式错误和未知意图不再清除活动任务。

#### R2.2 删除模型前业务终点

- 直接 handler 只保留为 Agent 可调用的只读 capability 或确定性 renderer；
- 删除 handler 在主 Agent 前生成最终业务回答、缺参罐头或“资源不存在”的职责；
- 对全部零主模型出口重新分类，只允许：协议失败、身份/权限失败、明确安全阻断和确认协议；
- 删除“需要上下文的直接回答意图”名单以及等价的第二份名单。

门禁：业务解析失败、registry 不完整、缺时间窗、缺实例和工具失败都不能绕过 Agent；`CanAssertAbsence=false` 时不得断言不存在。

#### R2.3 删除旧动作、槽位和 Prompt 补丁

- 删除 `taskSlotSpecs`、工作流专属 `parseUpdates/clarify`、生命周期关键词推断和整句参数正则；
- Workflow 只读取 `ExecutionContract`，不得读取原始用户文本；
- OperationSpec 只声明字段、类型、局部别名、结构化约束和 Workflow 绑定；
- 允许通用 Codec 和操作专属结构化校验器，但禁止用句式关键词代替 Agent 理解；
- 删除基础提示、意图提示、临时提示、工具说明和参数说明中的重复知识规则；
- 公共凭证清理只保留一个实现和一个策略作者源。

门禁：

- 新增普通操作不需要修改 Engine 的关键词、正则或意图 switch；
- `test/host/a`、`pytest`、ID 内的容量字符、多动作、多目标和参数纠正安全回归通过；
- 架构扫描确认旧符号和禁止新增模式在生产路径为零；
- 删除旧代码以后全量测试仍通过，且测试数量下降必须能对应到被删除的旧合同，不能掩盖安全覆盖下降。

### R3 / P9a：真实上下文最小验收

目标：用真实发生过的上下文问题证明新链路，不建设大规模主观评测。

固定已有生产记录中的最小集合，每条保留真实上一轮和本轮，不只截取短句：

1. “粘贴呢”：允许基于可靠上一轮直接回答；若检索，query 必须能独立理解；
2. 已检索到证据但旧守卫拒答：不得谎称知识库未覆盖；
3. 用户输入实例 ID 后继续诊断：不得再次追问是哪台；
4. 从列表按序号选择后继续；
5. 未完成写任务只补一个参数；
6. 用户纠正上一轮参数；
7. 用户放弃旧任务并开始新任务；
8. 旧部署任务不得劫持新的纯硬件创建；
9. registry 不完整时不得断言实例不存在；
10. 首轮确实没有上下文时允许合理澄清。

每条只检查可观察事实：进入 AgentContext 的字段、工具调用、是否生成 ActionProposal、是否执行写操作、最终答案是否使用已有信息。不得使用模型自评分数作为放行条件。

验收门：固定记录逐条通过；删除 AgentContext 接线、Response Gateway、Resolver 或确认 Gate 时对应样本必须失败。

### R4 / P9b：真实系统故障与前端验收

目标：证明上下文在传输、并发、重建和失败时仍连续。

执行矩阵：

| 场景 | 必须观察到的结果 |
|---|---|
| WebSocket 断线、刷新、重连 | 同一 Turn ID 继续重放，不重复执行 |
| 双标签页同时追问 | 会话执行权串行化，不产生两段历史 |
| 缓存逐出与冷建 | AgentContext 语义等价，活动租约不被逐出 |
| 进程重启与跨副本接管 | durable turn、任务状态、待确认动作可恢复 |
| 保存失败、CAS 冲突、数据库短暂故障 | 不发 `done`，前端明确显示 `TurnNotSaved` |
| 未知 schema | 可见降级、绝不覆盖原状态 |
| 损坏状态 | 可见重置并落库自愈 |
| 确认后 crash/takeover | journal 防止重复写，结果不确定时不冒充成功 |
| 真实只读查询 | 回答事实与上游结果一致 |
| 代表性创建、生命周期、扩容 | 最终资源状态与 journal 证明成功，并完成清理 |

执行要求：

- 前后端使用同一协议版本；
- 后端、WebSocket gateway 和当前 GitLab 前端进行真实联调；
- 有测试账号和权限时执行代表性写操作；缺少外部权限时，明确记录为发布阻断，不能用 mock 宣布通过；
- 成功以数据库、action journal 和最终资源查询为准，不以聊天文字为准。

### R5 / P9c：文档、旧 PR 和发布收口

- 更新主架构图、运行配置、工具开发、Workflow 开发和故障排查文档；
- 将旧 Router/direct-dispatch 文档移入历史说明或明确标记已废弃；
- 核对并关闭被 #454、#455–当前 PR 链替代的 #441–#452，避免误重放；
- 删除 observe-only、shadow、迁移 adapter 和过渡 feature flag；
- 在最终结果文档中列出：删除的旧入口、保留的安全门、真实回放结果、真实故障结果和无法在本地验证的外部依赖；
- 合并后从干净 `origin/main` 再跑一次全量门禁。

## 5. 全链路审计矩阵

最终验收必须逐层给出证据，不能只证明“历史在数据库里”。

| 层 | 必须成立的合同 | 主要证据 |
|---|---|---|
| 写入 | 答案、消息、任务状态同一提交；失败不假成功 | durable turn / CAS / fault tests |
| 清理 | 过期变成明确 notice 或重新确认，不静默变成可信事实 | Context Compiler tests |
| 逐出 | 使用中的会话不逐出；冷建语义等价 | pool pinning / rehydrate tests |
| 压缩 | 活动目标、约束、决定、待办和来源仍可见 | compaction fixtures |
| 传输 | Turn ID、序号、重放和前端失败态一致 | real WebSocket + frontend |
| 消费 | 普通业务先进入中心 Agent；无关键词门 | runtime mutation + real replay |
| 检索 | Agent 自主决定是否检索；检索时使用完整语义问题 | trace + knowledge fixtures |
| 工具 | Observation 完整、结构化并标明截断/时效 | Tool Gateway tests |
| 写操作 | 提案不等于授权；只经 Resolver/Gate/Workflow | action safety tests |
| 回答 | 证据和确定性事实不被错误删除或自由改写 | Response Gateway tests |

## 6. 禁止项

后续 PR 不得新增：

- 根据“这个、那个、呢、多少钱”等词决定是否传上下文；
- 根据“选择、决定、确认、改成”等词决定是否保存长期记忆；
- 第二个 Router LLM、ContextDecision LLM 或意图专属上下文包；
- 另一个“需要上下文的意图”名单；
- Workflow 从原始整句重新猜动作或参数；
- 为通过单个真实样本增加的新罐头、单句正则或写死映射；
- 把用户原话当成事实证据；
- 以 feature flag 长期维持两套语义系统。

允许保留的是严格协议解析、实体格式校验、单位转换、权限、确认、幂等、敏感信息清理、上游参数约束和确定性数据渲染。这些规则限制执行和证据，不决定 Agent 是否获得上下文。

## 7. PR 顺序与合并规则

| 顺序 | PR 内容 | 基线 | 可否并行 |
|---:|---|---|---|
| 1 | R1：生产入口和旧开关封死 | 当前 `origin/main` | 否，当前工作区继续完成 |
| 2 | R2.1：删除旧模型决策器 | R1 合并后的 `origin/main` | 否 |
| 3 | R2.2：删除模型前业务终点 | R2.1 合并后的 `origin/main` | 否 |
| 4 | R2.3：删除旧动作/槽位/Prompt 补丁 | R2.2 合并后的 `origin/main` | 否 |
| 5 | R3：真实记录最小回放 | 可随 R2 增补 fixture，但最终门在 R2.3 后 | 仅测试数据可并行 |
| 6 | R4：真实系统与前端验收 | 所有代码 PR 合并后 | 否 |
| 7 | R5：文档、旧 PR、发布收口 | R4 通过后 | 否 |

每个代码 PR 必须：

1. 从上一个已合并阶段的最新 `origin/main` 开分支；
2. 说明删除了哪条旧语义职责，不只列新增接口；
3. 有至少一个删除接线或恢复旧行为就会失败的业务断言；
4. 运行定向测试、`go test ./... -count=1`、`go vet ./...` 和架构扫描；
5. CI 全绿后再合并，合并后下一阶段重新基于 main 开始。

## 8. 最终完成定义

只有以下条件全部满足，才能宣布上下文遗忘的已知架构路径完成收口：

1. 所有普通业务轮次只进入一个 AgentRuntime；
2. AgentContext 是主模型唯一语义上下文入口；
3. 独立 Router LLM、ContextDecision LLM 和生产切回开关已删除；
4. 除协议、安全和确认外，没有零主模型调用的业务终点；
5. RAG、查询和写操作由同一个 Agent 基于同一上下文选择；
6. 写操作只能经 Proposal、Resolver、Gate 和 Workflow；
7. Workflow 不读原始聊天，旧关键词、整句正则和重复槽位作者源退出生产路径；
8. 回答、任务状态和执行结果使用统一提交与 Response Gateway；
9. 真实记录最小回放全部通过；
10. WebSocket、双标签页、逐出、重启、跨副本、数据库故障和前端失败态验收通过；
11. 代表性真实写操作由最终资源状态与 journal 证明；
12. 所有 shadow、adapter、旧 flag 和旧 PR 链完成删除或关闭。

完成以上条件后，仍可能出现模型判断错误或知识库缺失，但不应再出现“上下文明明存在，却因另一条业务旁路根本没给 Agent 看”的系统性失忆。
