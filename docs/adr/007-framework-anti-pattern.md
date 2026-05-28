# ADR-007: Anti-Pattern — Why Not Use an Agent Framework

**Status**: Proposed (2026-05-29)
**Depends on**: ADR-001 / ADR-006(本 ADR 防御性论证,无功能新增)

## Context

8 批落地计划(B1-B8)累计约 2500-3000 行 Go 新增 / 改造。team review 时合理 challenge:**"为什么不引入成熟的 agent framework(Eino / LangGraph / Claude Agent SDK)省工作量?"**

本 ADR 显式记录不引入这些 framework 的理由,作为未来类似建议出现时的常驻 reference。

业界头部 cloud agent(AWS Q / Bedrock 后端 / HolmesGPT / Devin)**没有一家用第三方 graph framework**,均自写 orchestrator。这条事实是本 ADR 的核心论据(memory `no-graph-framework-for-agent`)。

## Decision

8 批 B1-B8 全部自写 Go 实现,**不引入** Eino / LangGraph / Claude Agent SDK 中的任何一个。Reference pattern(saga / FSM / progressive disclosure / k8sgpt analyzer / OpenAI Function spec / MCP)**借鉴**,但**不引入 framework 抽象层**。

## 各候选 framework 的具体不采纳理由

### 1. Eino(Go,字节跳动 CloudWeGo,Apache-2.0)

- **stars**: 11.5k,主要中文圈采用,英文社区采用率低
- **核心定位**: graph orchestration / chain composition
- **痛点不匹配**: 我们的痛点是 **prompt 工程**(system prompt 5.9k 雪崩 / verbatim contract / progressive disclosure)+ **mutating workflow saga**,**不是** graph orchestration。Eino 解决的是相邻问题
- **生态薄**: bug 修复依赖字节单一公司,文档主要中文
- **抽象层 ≠ 简化**: 引入 framework 后我们的 orchestrator 仍要写 saga / HITL / step trace / idempotency / SSH sandbox,只是包了一层 framework 调用,代码量没明显下降还多了一层 cognitive load

### 2. LangGraph(Python,LangChain)

- **stars**: 33k+,英文社区主流 graph agent framework
- **Python 锁定**: 跟我们 Go 1.22 主线不匹配
- **集成成本**: 需要 sidecar + 重写 STS / MySQL / SSE bridge / `internal/llm` proxy,~1500 行胶水成本
- **Python 运维**: 部署多一种语言运行时,GC / 并发模型 / 资源占用都跟现有 Go 二进制单文件部署冲突
- **不抵收益**: 即使引入,saga / HITL / SSH sandbox 仍要自写(LangGraph 不解决这些)

### 3. Claude Agent SDK(Python/TypeScript,Anthropic)

- **Vendor 锁定 Claude only**: 我们 ADR-002 决定 fast / knowledge tier 走 ds-v4-flash,agent tier 走 ds-v4-pro,**完全不需要 Claude 作为主推理模型**
- **合规已不是顾虑**(ModelVerse 路由,见 memory `claude-code-provider-routing`),但模型锁定 con 仍在
- **作 reference 借鉴 OK**: handoff / guardrail / streaming pattern / context management 这些 pattern 移植成 Go,**不引入 SDK**
- **跟 ADR-005 B 路线区别**: B 路线是 **Claude Code subprocess(用户 CLI 工具)** 作为 diagnosis sub-agent,**不是** Agent SDK 框架。subprocess 是黑盒委派,SDK 是抽象层引入,本质不同

### 4. Bedrock / Vertex / Copilot Studio 框架 SDK

- **Vendor 锁定云厂商**: 我们是 CompShare 平台,引入 AWS/GCP/Azure SDK 商业逻辑层冲突
- **Reference pattern 借鉴**: Bedrock Action Group OpenAPI 形式 → ADR-003 tool spec;Copilot Topics → ADR-004 skill bundle;但不引入 SDK

### 5. Multi-agent collaboration framework(OpenAI Swarm / Bedrock Multi-Agent)

- **Q1 范围外**: ADR-001 三 tier 单 agent 已经足够覆盖 chatbot-grade + agent-grade 全部场景
- **未来真需要时考虑**: 多 agent 协作(e.g. orchestrator agent + specialist agents)是 Q3+ 议题,届时再评估
- **不预先抽象**: 全局 CLAUDE.md Rule 2 "Simplicity First" 适用

## 反向论证:什么情况会改变决策

为了避免本 ADR 变成永久 dogma,显式列出 framework 引入的触发条件。任一满足时本 ADR 应被 amend:

1. **B6 orchestrator 实际写完后超 2000 行,且 maintenance 成本明显高于 framework 集成成本**
2. **Multi-agent collaboration 成为产品需求**(e.g. 用户需要多个 specialist agent 协作)
3. **业界出现 Go-native + 解决我们具体痛点(prompt 工程 + saga + HITL)的 framework**,且 stars > 5k 半年内增速 > 50%
4. **团队规模扩到 5+ Go engineer 专职 agent 模块**,自写维护成本相对 framework 学习成本反转

## 业界对照

| 项目 | 用 framework? | 自写 orchestrator? |
|---|---|---|
| AWS Q Developer CLI | ❌ | ✅ Rust |
| AWS Bedrock Agent backend | ❌ | ✅ Java |
| HolmesGPT | ❌ | ✅ Python(declarative toolset + 自写 loop) |
| Devin | ❌(已知) | ✅ |
| Cursor agent mode | ❌(已知) | ✅ |
| Claude Code(本身) | ❌ | ✅ TypeScript |
| Anthropic Computer Use cookbook | ❌ | ✅ ref(demo,非 production orchestrator) |

**0 家头部 cloud agent 用第三方 graph framework**。这条数据本身就是最强反对论据。

## Consequences

**Positive**
- 无 framework 锁定,长期可维护性强
- ~2500-3000 行 Go 全部业务域代码,可读性 / debug 性 / hire 友好度都高于 framework 抽象层
- 工程债边界清晰(每个 internal/<module>/ 都是我们写的)

**Negative**
- 短期工作量高于"装个 framework 一周搞定"的乐观估计
- 新人 onboarding 看不到熟悉的 framework 名字,需要单独读 ADR + 代码

**Risks**
- **被业界范式转移甩开**: 5 家头部都自写不代表永远对,Q3 视行业演化重审本 ADR
- **自写质量 < framework 质量**: 缓解靠 ADR-006 acceptance + N=20 回归 + code review

## Acceptance

- [ ] 本 ADR 进 `docs/adr/README.md` 入口(若 README.md 不存在则同时建立 7 个 ADR + amendment 索引),新人入职 review
- [ ] B6 orchestrator 实施完成时 grep 仓库,确认无第三方 agent framework 依赖(`go.mod` 不含 eino / langchain-go-port / langgraph-go 等)
- [ ] **B6 实施完成时立即触发 ADR-007 Q3 重审**(governance 强制:不论行数是否真超 trigger 1 的 2000 阈值,B6 落地是 anti-pattern 决策的天然 review 点;Q3 同步 reassess trigger 1 阈值是否仍合理 + maintenance 成本评估)
- [ ] Q3 重审会议中,本 ADR 列入 review 议题

## References

- memory `no-graph-framework-for-agent`: 核心论据来源
- memory `industry-agent-patterns-2026-05-28`: 5 家头部自写 orchestrator 数据点
- ADR-001 / ADR-006: 受本 ADR 防御的方案
- 全局 CLAUDE.md Rule 2(Simplicity First)/ Rule 7(Surface conflicts)
