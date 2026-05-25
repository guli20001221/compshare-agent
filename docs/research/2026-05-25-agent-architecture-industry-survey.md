# Agent 架构行业调研报告（2025-2026）

> 调研日期：2026-05-25
> 调研范围：Anthropic / OpenAI / Google / AWS 官方最佳实践 + 思想领袖观点 + 关键论文
> 目的：为 CompShare Agent 架构文档提供行业对标依据

---

## 一、各大厂 Agent 架构核心模式

### 1.1 Anthropic — "先 workflow 后 agent，能简单就不复杂"

**核心文档：** [Building Effective Agents](https://www.anthropic.com/research/building-effective-agents) (2024-12)

五种 workflow 模式（复杂度递增）：

| 模式 | 适用场景 | CompShare 对应 |
|---|---|---|
| **Routing** | 输入有明确类别（客服分流） | Planner intent 分类 → Phase-1 cutover |
| **Chaining** | 任务可分解为固定步骤 | Workflow engine (create/stop/start) |
| **Parallelization** | 子任务独立可并行 | stock 多 zone fan-out |
| **Orchestrator-Workers** | 子任务不可预测 | Diagnosis（动态决定查什么） |
| **Evaluator-Optimizer** | 有明确评估标准 | Grounded renderer citation check |

**最关键的一句话：**
> "最成功的实现不是复杂框架，而是简单可组合的模式。" Agent 用延迟和成本换性能——需证明这个 tradeoff 值得。

**Context Engineering ([链接](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents), 2026)：**
- Context = 稀缺的注意力预算，需要工程纪律
- 三大策略：Compaction（压缩）、Clearing（清理过时数据）、Memory（外存持久化）
- **Context rot**：token 数增加 → 模型召回准确率下降

**Agent Skills ([链接](https://www.anthropic.com/engineering/equipping-agents-for-the-real-world-with-agent-skills), 2025-12)：**
- 三层渐进式加载：L1 name+description (~80 tokens/skill) → L2 完整 SKILL.md → L3 references 按需
- 已被 33+ 产品采纳（Claude Code, Codex, Copilot, Cursor, Gemini CLI...）
- Skill = 目录 + SKILL.md，无 SDK、无 API、无构建步骤

**Tool Use ([链接](https://www.anthropic.com/engineering/writing-tools-for-agents))：**
- 像给初级工程师写 docstring 一样写 tool description
- 最常见失败：**tool set 膨胀** —— 覆盖太多功能导致歧义选择

### 1.2 OpenAI — "从单 agent 起步，按需升级"

**核心文档：** [A Practical Guide to Building Agents](https://cdn.openai.com/business-guides-and-resources/a-practical-guide-to-building-agents.pdf) (2025-04)

**Agents SDK 四个原语 ([链接](https://openai.github.io/openai-agents-python/))：**
- **Agent** = LLM + 指令 + 工具
- **Handoff** = agent 间控制权转移（对等专家分工）
- **Guardrail** = 输入/输出校验（乐观并发执行）
- **Tracing** = 全链路事件记录

**两种多 agent 模式：**
- **Agent-as-Tool**：中心 orchestrator 把子 agent 当工具调用，保留控制权
- **Handoff**：对等 agent 间转移控制权，接管者拿到完整历史独立运行

**关键建议：**
1. 单 agent + 增量加工具能覆盖大多数场景，不要过早引入多 agent
2. 三根支柱：强模型 + 明确工具定义 + 结构化指令
3. 企业 guardrail：HITL 审批 → 只读优先 → 回滚机制 → 实时监控

**Swarm 设计哲学（Agents SDK 前身）：**
- 轻量无状态 + 可控可测 + 协作优于全能
- 核心验证：显式控制 > 不透明自动化

### 1.3 Google — "确定性编排 + LLM 动态路由分层"

**ADK (Agent Development Kit) ([链接](https://google.github.io/adk-docs/))：**

8 种多 Agent 模式：Sequential Pipeline / Coordinator-Dispatcher / Parallel Fan-Out / Hierarchical Decomposition / Generator-Critic / Iterative Refinement / Human-in-the-Loop / Composite

核心编排原语：
- `SequentialAgent` — 固定顺序执行
- `ParallelAgent` — 并行执行
- `LoopAgent` — 条件循环
- `AutoFlow` — LLM 驱动动态路由

5 种 Skill 设计模式（Google ADK 团队）：

| 模式 | 核心 | 适用场景 |
|---|---|---|
| **Tool Wrapper** | 按需加载领域知识 | 框架编码规范、技术栈最佳实践 |
| **Generator** | 模板+风格指南强制一致性 | 标准化文档生成 |
| **Reviewer** | 检查清单独立维护 | PR 审查、安全扫描 |
| **Inversion** | Agent 先采访用户再动手 | 需求澄清、架构设计 |
| **Pipeline** | 严格顺序，确认前不得继续 | 多阶段生产、需人工检查点 |

**Gemini Cloud Assist ([链接](https://docs.google.com/gemini/docs/cloud-assist/chat-panel))：**
- 自动读取当前页面 URL + 可见文本 → page context
- 结合 project-level 资源理解生成回答
- **控制台 Agent 的标杆参考**

**A2A Protocol ([链接](https://a2a-protocol.org/latest/specification/), 2025-04)：**
- Agent Card（能力广告）+ Task（交换单元）+ JSON-RPC 传输
- 150+ 组织参与，捐赠 Linux Foundation

### 1.4 AWS — "Action Group + Knowledge Base + Guardrail + Policy"

**Bedrock Agents ([链接](https://docs.aws.amazon.com/bedrock/latest/userguide/agents-multi-agent-collaboration.html))：**
- Supervisor + Collaborator 模式（2025-03 GA）
- Action Group = 工具集合（OpenAPI schema）
- Knowledge Base = RAG 语料
- Guardrails = 模型输出过滤（管表达）
- Policy = Cedar 策略引擎（管行为，逐 action 鉴权）

**AgentCore (re:Invent 2025)：**
- Identity（OAuth 2.0 token 自动管理，避免 token 进 LLM context）
- Episodic Memory（记录 context/reasoning/action/outcome，跨 session 学习）
- Evaluations（13 个内置评估器含 tool selection accuracy）

**Amazon Q Developer ([链接](https://aws.amazon.com/q/developer/features/))：**
- 控制台内 account resource 感知（列实例/查账单）
- 支持 MCP 扩展外部工具

---

## 二、思想领袖关键观点

### Harrison Chase (LangChain 创始人)
> "生产 agent 不是通用 ReAct 循环，而是 **flow engineering** —— 定制化状态机。"
>
> **Context engineering 比 prompt engineering 重要** —— 管理好 agent 看到什么信息，比优化措辞更关键。

- [Sequoia 访谈](https://sequoiacap.com/podcast/context-engineering-our-way-to-long-horizon-agents-langchains-harrison-chase/)
- [ODSC Deep Agents 主题演讲](https://opendatascience.com/harrison-chase-on-deep-agents-the-next-evolution-in-autonomous-ai/)

### Lilian Weng (OpenAI)
> Agent = LLM + Planning + Memory + Tool Use。**规划能力是瓶颈** —— 子目标分解与自我反思决定 agent 上限，而非工具数量。

- [LLM Powered Autonomous Agents](https://lilianweng.github.io/posts/2023-06-23-agent/)

### Chip Huyen (AI Engineering 作者)
> **评估（eval）比架构更重要。** AI 用得越多，灾难性失败概率越高。评估框架必须先于功能开发。

- [AI Engineering (O'Reilly 2025)](https://www.oreilly.com/library/view/ai-engineering/9781098166298/)

### Simon Willison
> **确定性控制流 + LLM 判断 = 可靠 agent。** 纯 LLM 自主决策在生产环境不可接受。Agentic engineering ≠ vibe coding。

- [Agentic Engineering Patterns](https://simonwillison.net/guides/agentic-engineering-patterns/what-is-agentic-engineering/)

### Will Larson (Calm)
> **Progressive Disclosure** —— 只在上下文窗口中放入最少必要信息，按需加载。Skill 只先暴露描述，大文件先提取摘要再按需展开。

- [Progressive Disclosure and Large Files](https://lethain.com/agents-large-files/)

### Shunyu Yao (ReAct 作者)
> 后续提出 **CoALA** (Cognitive Architectures for Language Agents)：记忆模块 + 结构化动作空间 + 泛化决策流程。关键转向：从 "如何推理" 转向 "**如何在真实业务约束下可靠执行**"。

- [CoALA (arXiv:2309.02427)](https://arxiv.org/abs/2309.02427)
- [tau-bench (arXiv:2406.12045)](https://arxiv.org/abs/2406.12045)

---

## 三、关键论文

### SkillRouter (阿里巴巴, arXiv:2603.22455, 2026-03)
- 1.2B 参数 retrieve-and-rerank pipeline 做 skill 路由，可部署在笔记本 CPU
- **颠覆性发现：skill body 才是路由关键信号** —— 只靠 name+description 路由准确率暴跌 31-44 个百分点
- 启示：capability 的完整 body 应暴露给路由器

### BiasBusters (arXiv:2510.00307, 2025-10)
- 7 个 LLM 中均存在工具选择偏差 —— 偏好上下文中位置靠前的工具
- 缓解方案：先过滤相关子集再均匀采样

### arXiv:2512.08769 — 生产级 Agent 设计原则
1. **单 Agent 单 Tool**，消除多 tool 选择歧义
2. **确定性任务用纯函数**，不走 LLM 推理
3. 提示外部化存储于版本库，非技术人员可修改 agent 行为
4. MIT 2025 报告：仅 5% 企业 GenAI 系统进入生产

---

## 四、行业共识提炼（与 CompShare Agent 对标）

### 4.1 六个跨厂商共识

| # | 共识 | 来源 | CompShare 现状 |
|---|---|---|---|
| 1 | **确定性路由优先，LLM 兜底** | Anthropic Routing / Google AutoFlow / AWS Supervisor / Simon Willison | ✅ Phase-1 cutover + ReAct fallback |
| 2 | **单 agent 起步，按需扩展** | OpenAI 指南 / Anthropic "能简单就不复杂" | ✅ 单 Engine，无 multi-agent |
| 3 | **Context 是稀缺资源** | Anthropic Context Engineering / Harrison Chase / Will Larson | ⚠️ System prompt 较精简但未按 skill 拆分 |
| 4 | **Guardrail 必须分层** | OpenAI input/tool/output / AWS Guardrails+Policy / Anthropic | ✅ preblock + SafeToolExecutor + output redaction |
| 5 | **Eval 先于功能** | Chip Huyen / AWS AgentCore Evaluations / Anthropic | ✅ golden_test + shadow_qa + CLI eval |
| 6 | **Skill/Tool 描述是路由成败的关键** | Anthropic Tool Use / SkillRouter 论文 / BiasBusters | ✅ capability frontmatter + tool descriptions |

### 4.2 三个 CompShare 尚未对齐的方向

| # | 方向 | 行业做法 | CompShare 差距 |
|---|---|---|---|
| 1 | **Skill 化命名与文档** | Anthropic Skill 规范 / Google ADK 5 模式 / LangGraph node 命名 | capability 在代码中清晰但文档中缺乏业界术语映射 |
| 2 | **控制台页面上下文** | Google Gemini Cloud Assist page context / Amazon Q resource 感知 | 无 ConsoleContext（待前端配合） |
| 3 | **Episodic Memory** | AWS AgentCore / CoALA 记忆模块 | SessionState 有 ToolFact 但无跨 session 学习 |

### 4.3 CompShare 已领先业界基线的方面

| 方面 | 说明 |
|---|---|
| **Evidence-based rendering** | envelope.Envelope + grounded.Renderer 是独有设计，LangGraph/OpenAI SDK/ADK 均无等价物 |
| **RAG 作为一等路由路径** | 不是 tool，是独立的 dispatch 分支，比 Bedrock Knowledge Base 的集成更深 |
| **Multi-tenant governance** | per-tenant rate limiting + STS credential isolation，超过多数开源框架 |
| **Deterministic capability cutover** | 55-65% 请求绕过 LLM tool selection，直接确定性分发 |

---

## 五、框架对比表

| 维度 | LangGraph | OpenAI SDK | Google ADK | AWS Bedrock | **CompShare Agent** |
|---|---|---|---|---|---|
| **编排模型** | 有向图 + 条件边 | Handoff + Agent-as-Tool | 层级 Agent 树 | Supervisor + Action Group | **Planner Router + Capability Cutover + ReAct Fallback** |
| **状态管理** | Checkpoint + 时间旅行 | Context Var（瞬态） | Session.state | Episodic Memory | **SessionState + MySQL CAS** |
| **Tool 路由** | LLM conditional edge | LLM tool_choice | AutoFlow + deterministic | FM ReAct + custom | **Planner LLM + 确定性 handler** |
| **Human-in-the-loop** | interrupt() / Command | Guardrail + approval | 自定义 tool 暂停 | Graph interrupt_before | **ConfirmFunc (CLI stdin, HTTP TBD)** |
| **Guardrail** | 节点级 | Input/Output 并发 | Agent 级 | Guardrails + Cedar Policy | **3 层：preblock / SafeExecutor / output** |
| **Streaming** | Token callback | Token callback | Token callback | SSE | **SSE (meta/token/done/error)** |
| **可视化** | `draw_mermaid_png()` | 无 | 无 | 控制台图形 | **无（需补 Mermaid 文档）** |
| **语言** | Python | Python | Python | Python / API | **Go（编译型，性能优势）** |

---

## 六、参考链接汇总

### 官方文档
- [Anthropic: Building Effective Agents](https://www.anthropic.com/research/building-effective-agents)
- [Anthropic: Context Engineering](https://www.anthropic.com/engineering/effective-context-engineering-for-ai-agents)
- [Anthropic: Agent Skills](https://www.anthropic.com/engineering/equipping-agents-for-the-real-world-with-agent-skills)
- [Anthropic: Writing Tools for Agents](https://www.anthropic.com/engineering/writing-tools-for-agents)
- [OpenAI: Agents SDK](https://openai.github.io/openai-agents-python/)
- [OpenAI: Practical Guide to Building Agents](https://cdn.openai.com/business-guides-and-resources/a-practical-guide-to-building-agents.pdf)
- [OpenAI: Orchestration and Handoffs](https://developers.openai.com/api/docs/guides/agents/orchestration)
- [Google: ADK Documentation](https://google.github.io/adk-docs/)
- [Google: Gemini Cloud Assist](https://docs.google.com/gemini/docs/cloud-assist/chat-panel)
- [Google: A2A Protocol](https://a2a-protocol.org/latest/specification/)
- [AWS: Bedrock Multi-Agent](https://docs.aws.amazon.com/bedrock/latest/userguide/agents-multi-agent-collaboration.html)
- [AWS: Amazon Q Developer](https://aws.amazon.com/q/developer/features/)
- [LangGraph: Workflows and Agents](https://docs.langchain.com/oss/python/langgraph/workflows-agents)

### 思想领袖
- [Harrison Chase: Context Engineering (Sequoia)](https://sequoiacap.com/podcast/context-engineering-our-way-to-long-horizon-agents-langchains-harrison-chase/)
- [Lilian Weng: LLM Agents](https://lilianweng.github.io/posts/2023-06-23-agent/)
- [Simon Willison: Agentic Engineering Patterns](https://simonwillison.net/guides/agentic-engineering-patterns/what-is-agentic-engineering/)
- [Will Larson: Progressive Disclosure](https://lethain.com/agents-large-files/)
- [Chip Huyen: AI Engineering](https://www.oreilly.com/library/view/ai-engineering/9781098166298/)

### 论文
- [SkillRouter (arXiv:2603.22455)](https://arxiv.org/abs/2603.22455)
- [BiasBusters (arXiv:2510.00307)](https://arxiv.org/abs/2510.00307)
- [CoALA (arXiv:2309.02427)](https://arxiv.org/abs/2309.02427)
- [tau-bench (arXiv:2406.12045)](https://arxiv.org/abs/2406.12045)
- [Production Agent Design Principles (arXiv:2512.08769)](https://arxiv.org/html/2512.08769v1)

### 行业分析
- [Firecrawl: Agent Framework Comparison](https://www.firecrawl.dev/blog/best-open-source-agent-frameworks)
- [Augment: Multi-Agent Orchestration Guide](https://www.augmentcode.com/guides/multi-agent-orchestration-architecture-guide)
- [Arize: Orchestrator-Worker Comparison](https://arize.com/blog/orchestrator-worker-agents-a-practical-comparison-of-common-agent-frameworks/)
- [阿里云: Agent Skill 规范构建与设计模式](https://developer.aliyun.com/article/1734589)
