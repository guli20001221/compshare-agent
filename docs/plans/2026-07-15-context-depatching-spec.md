# 上下文连续性去补丁化重构规格

状态：Proposed  
基线：`codex/context-continuity-v2@66bd3dc7`  
目标读者：后端、前端、测试与发布负责人

## 1. 背景

当前整合分支已经解决了会话提交、跨进程并发、断线恢复、缓存逐出、知识证据保存和多类上下文消费问题，但最后一轮审计发现，部分修复仍依赖以下补丁式机制：

- 根据“这个、那个、还是、呢”等词判断回答是否需要上下文。
- 另建一份“需要上下文的直接回答意图”名单。
- 根据“就用、选择、决定、确认”等词猜测长期记忆中的决定。
- 在多个提示位置重复知识问答规则。
- 在语义核验之外，再用数字、单位、否定词正则判断答案是否可信。
- 在公共安全模块和轮次协调器中重复维护凭证正则。

这些机制短期能覆盖已知样本，但会产生三个长期问题：新表达漏判、新意图漏登记、多个规则副本逐渐漂移。本次重构不继续扩充词表，而是建立统一的上下文、回答、记忆、知识和安全契约。

## 2. 目标

完成后必须满足：

1. 是否读取上下文不由用户用了什么词决定。
2. 所有直接回答默认获得同一份理解上下文，不维护第二份意图名单。
3. 每条生产回答只能通过一种明确出口：Agent 回答、带证据回答、安全策略回答或明确失败。
4. 知识问答的行为规则只有一个来源，每轮只注入一次。
5. 长期记忆由结构化事件或带出处的压缩结果产生，不从关键词猜测。
6. 事实支持关系只由统一语义核验器判断；程序只检查结构和出处是否合法。
7. 凭证识别和清理只有一个公共实现。
8. 重构后删除旧旁路，不靠长期功能开关维持两套实现。

## 3. 非目标

- 不改变现有工具的业务能力和写操作确认边界。
- 不把用户原话当作事实证据。
- 不让过期实例、旧工具结果或长期摘要重新获得操作授权。
- 不重新引入终端 RAG 作为生产主路径。
- 不通过大规模人工评测决定基础正确性；行为评测只使用已有真实记录，安全反例使用自动化变异测试。

## 4. 核心设计原则

### 4.1 上下文是输入数据，不是语言特征

任何回答路径都从统一的 `TurnContextView` 取得所需视图。路径可以不使用其中某一字段，但不能因为当前句子“看起来完整”就不传上下文。

### 4.2 一个事实只登记一次

意图的执行方式、工具范围、回答方式和安全级别必须登记在同一个路由定义中。禁止在 Engine、Prompt、Handler 中分别维护名单。

### 4.3 语义交给模型，结构交给程序

程序负责检查 ID 是否存在、引用文本是否确实来自输入、JSON 是否完整、操作是否被授权。程序不再用中文词表或单位换算判断一句话是否与证据等价。

### 4.4 安全阻断不等于删除上下文

安全规则可以阻止回答或操作，但必须保存本轮、保留任务语义，并明确说明结果。只有用户明确开始新任务或任务完成时才能清理活动任务。

## 5. 目标架构

### 5.1 统一轮次上下文视图 `TurnContextView`

`e.messages` 继续是模型对话的唯一来源，SessionState 继续是结构化记忆的唯一来源。本重构不新增第三份上下文存储，也不要求 Handler 传递完整聊天记录。

新增一个进程内只读视图，由 Engine 在每轮开始时从上述两个来源构建一次：

```go
type TurnContextView struct {
    CurrentQuestion     string
    RecentConversation []ConversationPair
    ConversationDigest ConversationDigest
    ActiveTask          *TaskSnapshot
    SelectedEntities    []EntityHint
    VerifiedKnowledge   []VerifiedKnowledgeTurn
    ContinuityNotices   []ContinuityNotice
}
```

约束：

- `RecentConversation` 只包含已提交的完整问答对。
- `ConversationDigest` 和 `EntityHint` 只帮助理解，不授权写操作。
- `VerifiedKnowledge` 只能来自已通过统一证据核验的答案。
- `ContinuityNotice` 记录未知状态版本、未确认操作结果、只读降级等情况。
- Router、Agent、直接 Renderer 和知识回答器从同一对象投影自己需要的视图；业务 Handler 仍只接收业务请求，不接收完整聊天记录。
- 不新建第二套持久化；持久化来源仍是已提交消息和 SessionState。

### 5.2 单一路由注册表

将现有只读投影 `DispatchSpec` 提升为唯一的不可变分发契约，并从生成的 route/skill registry 构建它；Engine 不再维护平行名单：

```go
type DispatchSpec struct {
    Intent           Intent
    ExecutionMode    ExecutionMode
    ToolScope        tools.ToolScope
    ResponseContract ResponseContract
    SafetyClass      SafetyClass
}
```

`ResponseContract` 只允许：

- `ResponseAgent`：交给 Agent，使用完整 `TurnContextView`。
- `ResponseGrounded`：Handler 提供 EvidenceEnvelope，Renderer 同时接收 `TurnContextView` 的理解视图。
- `ResponsePolicyTerminal`：仅安全、合规或能力缺失规则可用；仍保存上下文。

删除：

- `contextAwareDirectIntents`
- `isContextAwareDirectIntent`
- 任何 Engine 内另建的直接意图名单

迁移分两步完成：先让新契约与现有真实分发结果做逐项一致性检查，再把 Engine 消费点切到该契约，最后删除旧投影和散落映射。注册表覆盖测试必须证明：每个生产意图只登记一次，并且工具范围、执行方式和回答契约都非空。

### 5.3 直接回答统一契约

直接 Handler 不再返回“只有一句字符串但不知道是否读过上下文”的结果。

规则：

1. `HandlerStatusHandled` 必须带 EvidenceEnvelope。
2. 确定性字符串结果统一包装成 `computed_result` 证据，不再只在命中特定中文词时包装。
3. Renderer 总是收到 `TaskSpec`，该 `TaskSpec` 从 `TurnContextView` 生成，不按意图名单裁剪。
4. `NeedsClarification` 且存在历史问答时，一律回到受限 Agent 处理；不再判断当前句子是否含有指代词。
5. 首轮确实缺少业务参数时，Handler 可以直接返回结构化澄清。
6. 安全策略回答必须使用独立的 `PolicyTerminalResult`，不能伪装成普通 Handler 回答。

删除：

- `hasContextDependentDirectSignal`
- “这个、那个、还是、呢、多少钱”等词表
- `contextEnvelopeForPlainDirectReply` 中基于词表的条件

### 5.4 知识问答单一策略来源

新增一个 `KnowledgeTurnPolicy`，由 Prompt Builder 按 section ID 注入一次：

```go
PromptSection{
    ID: "knowledge_turn_policy",
    Text: ...,
}
```

Prompt Builder 对 section ID 去重；重复 ID 直接测试失败。

知识轮只保留以下规则：

- 先读取 `TurnContextView` 中对当前模型可见的对话与记忆。
- 已验证的前文足以回答时可以直接回答。
- 需要新事实或时效确认时，生成脱离上文也完整的 query 后调用 `SearchKnowledge`。
- 不得补造对话和证据中不存在的条件。

调整范围：

- 从通用 `segmentKnowledgeBoundary` 删除操作步骤，只保留知识来源边界。
- 从 `segmentIntentScopedMutatingRules` 和旧 `segmentMutatingRules` 删除重复的 knowledge_qa 段落。
- `knowledgeQAAgentLoopSearchNote` 只保留本轮状态差异，例如“本轮尚无可复用证据，首次检索为必需”，不再重复完整策略。
- `SearchKnowledge` 工具说明只描述用途、输入和输出；query 参数只写“独立可理解的问题”，不重复整段行为规则。
- 工具调用重试不得再次附加相同策略。

生产知识问答固定走 Agentic RAG。生产配置不再允许切回终端 RAG：删除旧开关，或在启动时拒绝关闭 Agentic RAG 的生产配置。终端 RAG 只允许在明确的组件兼容测试中存在；完成调用关系核对后，应删除生产分流、旧 cite-only guard 和孤儿补全函数，而不是继续同步两套策略。

### 5.5 统一知识答案核验

保留一个语义核验器，输入为：

- resolved question
- candidate answer
- EvidenceLedger

输出为带出处的结构化结论：

```json
{
  "supported": true,
  "claims": [
    {
      "answer_quote": "...",
      "chunk_id": "...",
      "evidence_quote": "..."
    }
  ],
  "unsupported": []
}
```

程序只检查：

- 输出格式完整。
- `answer_quote` 确实出现在答案中。
- `chunk_id` 确实在本轮或已验证历史账本中。
- `evidence_quote` 确实出现在对应证据中。
- 核验器明确给出 supported/unsupported。

程序不再判断：

- 数量和单位是否等价。
- 否定词是否反转。
- 中文分句是否覆盖所有主张。
- 某个标点是否构成引用。

删除：

- `knowledgeClauseBoundaryRE`
- `groundingPolarityNoiseRE`
- `groundingQuantityRE`
- `groundingNegationReplacements`
- 单位归一化映射
- cite-or-refuse 的标点门

核验失败时只允许一次带证据修复；修复结果必须携带同样的结构化出处。两次均失败则明确说明“当前证据无法支持可靠答案”，不得谎称没有检索到资料。

任何知识草稿必须在核验完成后才开始向前端发送。

这不会给所有轮次增加模型调用：只有准备输出知识结论且现有确定性证据契约不能直接证明时才调用核验器。已有历史证据足够时可以不再次检索，但仍需使用同一核验契约审视最终答案。

### 5.6 长对话压缩与长期记忆

删除从用户文本关键词推断“决定”的逻辑。长期记忆分两类来源：

#### A. 业务结构化事件

以下事件直接更新 `TaskSnapshot` / `ConversationDigest`：

- 用户确认的表单修改。
- 上下文裁决层确认的任务继续、新任务或任务结束。
- 工作流缺少参数、完成、失败或取消。
- Handler/工具确认的实体和事实。
- 通过核验的知识答案。

这些更新不需要额外模型调用，也不解析自然语言关键词。

#### B. 被逐出问答的语义压缩

只有在完整问答即将从活动窗口逐出时调用一次结构化压缩器。输入包含：

- 即将逐出的完整问答对。
- 当前 ConversationDigest。
- 当前 TaskSnapshot。

输出必须包含来源：

```go
type MemoryDelta struct {
    Goals           []SourcedMemory
    Constraints     []SourcedMemory
    Decisions       []SourcedMemory
    UnresolvedTasks []SourcedMemory
}

type SourcedMemory struct {
    Value     string
    PairIndex int
    Quote     string
}
```

程序检查 `Quote` 是否逐字存在于对应问答对。没有合法出处的条目不保存。

压缩器由轮次协调层在提交边界调用，通过 MessageStore 从“摘要前沿”开始分页读取已提交的完整问答对；Engine 不直接访问数据库。摘要、原文摘录、摘要前沿和 SessionState 必须在同一次提交中落库，禁止先移动前沿再保存摘要。

压缩失败时：

- 不生成猜测摘要。
- 保留现有结构化任务记忆。
- 将最近一小段被逐出完整问答保存为“原文摘录”，不贴目标/决定等语义标签。
- 记录 `compaction_summary_failed`，下次可从数据库中的完整历史重新尝试。

SessionState 升级到新版本时必须保留现有未知 schema 的只读保护规则。摘要前沿只有在摘要或原文摘录成功保存后才能前移。

删除：

- `absorbConversationDigest` 中“就用、选择、决定、改成、换成、确认”词表
- 所有通过 `containsAnyKeyword` 给长期记忆分类的逻辑

### 5.7 凭证保护统一

`internal/guardrails` 成为唯一凭证规则所有者，公开：

```go
func RedactCredentials(string) string
func ContainsCredential(string) bool
```

轮次协调器、消息保存和输出清理只能调用这两个接口，不得自行定义凭证正则。

删除：

- `turncoord.shortCredentialMarker`
- `redactTurnOutput` 中第二次凭证替换
- 其他包内重复的 AK/SK/token/password 模式

公共测试必须覆盖：幂等、私钥、JWT、Authorization、AK/SK、普通实例 ID、项目 ID、邮箱、IP 和自然语言不被误删。

## 6. 生产出口不变量

每一轮只能以以下一种结果结束：

| 出口 | 必须具备 | 是否保存上下文 |
|---|---|---:|
| AgentAnswer | 完整 TurnContextView、允许的工具范围 | 是 |
| GroundedAnswer | EvidenceEnvelope、TaskSpec、核验结果 | 是 |
| PolicyTerminal | 明确安全/能力原因 | 是 |
| TurnFailure | 明确未提交，前端不得继续假装成功 | 不产生伪历史 |

禁止存在第五类“直接返回一句字符串但无法证明读过什么”的生产出口。

## 7. 可观测性

每轮 trace 只记录有界元数据，不保存原始提示或敏感内容：

- `context_sources`：recent_pairs / digest / active_task / verified_knowledge / notices
- `response_contract`：agent / grounded / policy_terminal / failure
- `prompt_section_ids`：实际注入的 section ID
- `memory_update_source`：structured_event / compactor / excerpt / none
- `grounding_outcome`：supported / repaired / unsupported / unavailable

验收查询必须能回答：

- 哪条回答没有读取任何上下文来源？
- 哪个 Prompt section 重复注入？
- 哪条长期记忆没有来源？
- 哪个直接 Handler 没有 EvidenceEnvelope？

## 8. 实施顺序与 PR 拆分

所有重构先从 `codex/context-continuity-v2` 新开 `codex/context-depatching-v1`，不向旧 #441–#452 继续叠加。

### PR D1：统一回答与路由契约

- 引入 `TurnContextView`、`ResponseContract`。
- 路由注册表成为唯一来源。
- 所有直接 Handler 无条件获得 TaskSpec。
- 删除直接意图名单和短句关键词表。
- 增加注册表完整性、无 Envelope 不得完成的测试。

### PR D2：知识提示单一来源

- Prompt section ID 去重。
- 合并 knowledge_qa 行为规则。
- 精简 SearchKnowledge 工具说明。
- 删除重复临时提示。
- 核对并退役生产终端 RAG 分流。
- 删除可让生产重新启用终端 RAG 的配置退路，并增加启动配置测试。

### PR D3：知识核验去正则化

- 保留单一语义核验器和结构校验。
- 删除数量、单位、否定词、分句和引用标点规则。
- 保留一次带出处修复。
- 使用真实知识拒答记录做小规模回放。
- 用自动变异构造“数字改变、条件遗漏、否定反转、证据 ID 伪造”，证明语义核验器会拒绝；这些反例只存在于测试，不进入生产词表。

### PR D4：长期记忆结构化

- 删除关键词推断。
- 业务事件直接更新记忆。
- 增加带出处的稀有压缩调用和原文摘录降级。
- 升级 SessionState，验证冷/热、重启和摘要前沿一致。

### PR D5：凭证规则统一

- 增加公共 `ContainsCredential`。
- 删除 turncoord 重复正则。
- 验证恢复请求、回复保存和 trace 的清理一致。

### PR D6：组合验收与前端同步

- 合并 D1–D5 后运行真实 PostgreSQL 和 WebSocket 套件。
- 同步前端持久轮次分支。
- 验证刷新、断线、双标签页、重启、逐出和数据库故障。
- 通过后关闭或标记旧 #441–#452 为 superseded。

## 9. 测试与验收

### 9.1 必须自动化通过

- `go test ./... -count=1`
- 真实 PostgreSQL：store / turncoord / httpapi
- 前端现有 36 项协议测试
- Prompt section 唯一性测试
- 生产配置无法关闭 Agentic RAG 的启动测试
- 路由注册表完整性测试
- 所有 Handled 结果必须有 EvidenceEnvelope 的测试
- SessionState 新旧版本与未知版本测试
- 删除接线点后必红的变异测试

### 9.2 上下文样例

必须覆盖不依赖关键词的表达：

- “换成包月”
- “按 8 卡算”
- “第二种”
- “继续”
- “同配置再看上海”
- “这次别重启”

这些句子不能依赖词表命中；上下文必须始终进入 Router、Agent 或 Renderer。

### 9.3 真实记录回放

只使用仓库已有的真实记录或其脱敏副本：

- “粘贴呢”及其完整上一轮。
- 31 条“知识库未覆盖”及对应 retrieval trace。
- 诊断续问、监控时间续问、任务确认和直接查询续问。

验收指标：

- 原本有证据的回答不得因标点、数字表达或否定词表被删除。
- 无证据回答不得通过语义核验。
- 数字改变、条件遗漏、否定反转和伪造证据 ID 的自动变异必须被拒绝。
- 未调用 RAG 但由已验证历史充分支持的回答可以通过。
- 下一轮必须能继续使用本轮已提交结果。

不使用总 abort 率作为单独门槛；它包含大量与上下文无关的首轮样本。

## 10. 删除清单

完成 D1–D5 后，下列实现必须不存在：

- `contextAwareDirectIntents`
- `hasContextDependentDirectSignal`
- 上下文相关中文关键词表
- 长期记忆决定词表
- 重复 knowledge_qa 行为段落
- `knowledgeClauseBoundaryRE`
- `groundingPolarityNoiseRE`
- `groundingQuantityRE`
- `groundingNegationReplacements`
- 单位归一化 switch
- `shortCredentialMarker`
- 生产终端 RAG 和 cite-only 分流（若调用关系核对无生产依赖）

删除清单是验收条件，不是后续清理建议。

## 11. 回滚策略

- 不新增长期运行时开关。
- 每个 PR 独立可回滚，但同一 PR 内不保留新旧双路径。
- 数据 schema 只做向前兼容增加；旧版本读到新状态时继续使用现有只读保护。
- D6 发布失败时整体回滚二进制，不能关闭持久轮次后退回旧 10 轮兼容路径。

## 12. 完成定义

只有同时满足以下条件才可宣布去补丁化完成：

1. 删除清单全部完成。
2. 生产回答出口全部符合四类契约。
3. Prompt 中 knowledge policy 只出现一次。
4. 新增意图不需要在 Engine 维护第二份上下文名单。
5. 长期记忆每个语义条目都有结构化事件或原文出处。
6. 凭证规则只存在于 guardrails。
7. 自动化、真实记录回放和真实 WebSocket 验收全部通过。
8. 前后端同批上线，持久轮次不能静默降级。
