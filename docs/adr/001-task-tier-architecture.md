# ADR-001: Task-Complexity Tiered Architecture

**Status**: Proposed (2026-05-28)
**Author**: 联合架构调整
**Supersedes**: 当前 `internal/engine/engine.go` 的混合 dispatch(Phase-1 cutover + ReAct fallback + RAG)

## Context

Compshare Copilot 服务的用户请求质量差异极大:

| 请求示例 | 复杂度 |
|---|---|
| "我有哪些实例" / "4090 多少钱" / "现在有货吗" | 单 API 查询 |
| "怎么用 SecurityToken 签名" / "Qwen vs DeepSeek 区别" | 文档问答 |
| "帮我部署 Qwen32B" / "SSH 进去看下 GPU 为什么不识别" | 多步规划 + 副作用 |

当前架构用单一 ReAct loop + capability cutover 混合 dispatch 同时服务三类,导致:
- 弱模型(ds-v4-flash)在复杂任务上分类雪崩(N=10 oracle 验证 input_tok>11k 全崩)
- 强模型成本浪费在简单查询上
- 多步任务无法暂停/回滚/审计

业界 5 家头部 cloud agent 均按任务复杂度分层路由 + 模型分流(见对照表)。

## Decision

引入 3 个 first-class tier,planner 路由到对应 tier 的 dispatch 管线:

| Tier | 触发 | Dispatch 管线 | 模型 |
|---|---|---|---|
| **fast** | 单 API 查询/读类 | handler + template render | ds-v4-flash |
| **knowledge** | 平台问答/FAQ/概念 | RAG + LLM compose | ds-v4-flash |
| **agent** | 多步任务/部署/排障 | orchestrator + tool loop + HITL + rollback | ds-v4-pro 或同等强模型 |

Tier 选择是 planner 输出的 first-class 字段,trace 落 `task_tier` 列,observability/计费/限流均按 tier 分桶。

## Consequences

**Positive**
- 模型成本下降(chatbot-grade 请求继续走 flash)
- Agent path 跑强模型,复杂任务质量提升
- 不同 tier 的失败模式独立(fast path renderer fab 不再污染 agent path)

**Negative**
- Planner 输出 schema 重写为 `{tier, skills[], slots, tool_calls[]?}`,需 N≥20 回归
- `internal/engine/engine.go` dispatch 从 if-else 拆 3 路 first-class
- 模型路由层(`internal/llm/router.go`)新增,~100 行

**Risks**
- Planner 三分类边界模糊("我有 4090 4 卡机器吗" = fast 还是 agent?)→ 缓解:fallback 倾向 `fast > knowledge > agent`,误判到 agent 是浪费,反之是降级体验
- Planner jitter(同输入多次分类不一致):弱模型 ds-v4-flash 在 borderline 输入上稳定性 < 100%(memory `jitter-check-for-classification`)→ 缓解:N≥5 同输入 oracle smoke 锁定 borderline 集,加入 planner few-shot 反例库
- fast / knowledge 边界模糊("镜像分类"是 fast list query 还是 knowledge FAQ?)→ 缓解:planner 输出附 `tier_reason` 字段 + trace 落库 review;策略上 favor knowledge(grounded renderer 比 template fab 风险低)

## 业界对照

| 平台 | Fast(查询/读) | Knowledge(问答) | Agent(任务) |
|---|---|---|---|
| AWS Bedrock | Action Group(typed API) | Knowledge Base + RAG | Bedrock Agent ReAct |
| AWS Q Developer | Nova-lite quick lookup | RAG with KB index | Q Developer Pro → Claude |
| OpenAI Assistants | Function Calling | File Search + Vector Store | Code Interpreter / Run loop |
| Anthropic Claude.ai | Tool Use(typed) | Citations + 文档检索 | Extended Thinking / Computer Use |
| MS Copilot Studio | Topics(typed flow) | Generative Answers(RAG) | Agent Flows |
| **本项目** | handler + template | RAG + LLM compose | orchestrator + saga + HITL |

三层在 5 家头部 platform 均独立 first-class,本架构与之对齐。

## Acceptance

- [ ] Planner 输出 `task_tier` 字段,trace 落库
- [ ] `internal/engine` 三 path dispatch 分离,无 fallback 互窜
- [ ] `internal/llm/router.go` 按 tier 选模型,配置在 `agent.yaml.tier_config`
- [ ] N=20+ 真实问题回归,tier 分类误差 < 10%(label 标准:annotator 按本 ADR Decision 表 dispatch 管线对应关系标 ground truth;分类误差 = count(planner_tier ≠ ground_truth_tier) / N;borderline case 走多数投票,3 人 disagree 标 ambiguous 不入分母)

## References

- ADR-002: Model routing per tier(2026-05-29 完成)
- ADR-003: Skills and Tools as orthogonal dimensions
- ADR-006: Agent path 6 hard requirements(2026-05-29 完成)
- ADR-007: 反方案讨论(2026-05-29 完成)
