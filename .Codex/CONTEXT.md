---
last_updated: 2026-05-12T16:30:00+08:00
---
# Project Context
## 当前状态
当前分支 `codex/pre-rag-finance-routing` 位于 `F:\compshare-agent-worktrees\pre-rag-finance-routing`，基于 PR #64 后的 `origin/main`。PR-B 财务问题分流 + FAQ 补全已实现并通过本地验证，待提交/开 PR。

## 进行中
- [x] PR-B 财务分流 — 账号实时财务 hard-block、通用财务 FAQ 走 knowledge_qa、实例级计费 pass-through。

## 最近完成
- [x] 财务 hard-block 扩展 — 覆盖发票状态、开票/审核通过、退款进度/成功/到账、欠费金额、待支付账单、扣费/交易记录；混合实时财务问题整体引导财务中心。
- [x] 财务 FAQ 补全 — curated FAQ 增加开发票、欠费处理、计费方式区别、退款规则、套餐到期与续费。
- [x] Planner 提示词 — 明确 finance FAQ / account realtime / instance billing 三类路由。
- [x] Knowledge retriever — product_area 不能单独命中，必须有问题文本匹配，避免财务 FAQ 噪音。
- [x] 子 agent review — plan review 提出欠费/混合句补充；code review 提出实时财务漏口；复审提出“发票申请流程”误拦，均已修复。

## 架构决策
| 决策 | 选择 | 原因 | 日期 |
| --- | --- | --- | --- |
| 财务三分流 | 账号实时财务 hard-block；通用规则 FAQ；实例计费 pass-through | 防止助手查询无权限账号数据，同时让开发票/退款规则可由知识库回答 | 2026-05-12 |
| 混合实时财务 | 只要同句包含实时账号财务，整体引导财务中心 | 不拆半句回答，避免用户误以为助手可查状态/金额 | 2026-05-12 |
| 检索打分 | product_area 只加分，不能单独命中 | 避免一个 billing 问题捞出所有财务 FAQ | 2026-05-12 |

## 已知问题
- [medium] 当前 FAQ 是 curated 最小集，完整 RAG 语料仍需后续导入 GitLab/飞书知识库。
- [medium] 真实账号财务状态仍不查询，统一引导控制台财务中心，这是当前产品边界。
