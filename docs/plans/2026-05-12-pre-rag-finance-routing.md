# PR-B: 财务问题分流 + FAQ 补全

## 目标

在进入完整 RAG 前，先把财务类问题分清楚：

- **实例级计费**：例如“这台为什么还在扣费”“哪些停机实例仍可能计费”“全实例计费方式汇总”。这些问题可以继续走现有实例 API / ReAct 路径，不应被账号级 hard-block 拦掉。
- **账号实时财务**：例如“账号余额”“本月总账单”“消费流水”“我的发票状态”“退款进度”“我欠费了吗”“欠费金额”“待支付账单”“发票寄送/开票进度”。这些信息当前不由助手查询，必须直接引导用户去控制台财务中心。
- **通用财务规则 FAQ**：例如“如何开发票”“欠费怎么办”“退款规则是什么”“按量和包日有什么区别”“套餐到期怎么办”。这些是知识库问题，应走 `knowledge_qa` / curated FAQ。

## 不做

- 不接完整外部 RAG pipeline。
- 不查询账号级实时财务 API。
- 不改变实例计费诊断工具的行为。
- 不新增 mixed intent。
- 不把“如何开发票”这类规则问题 hard-block。

## 设计

### 1. 财务问题三态分类

在 engine 层新增窄分类，服务于账号级 hard-block：

- `account_realtime`：余额、总账单、消费流水、发票状态、退款进度、退款到账等用户个人实时财务状态。
- `general_rule`：开发票方法、退款规则、欠费规则、计费方式区别、套餐到期/续费规则。
- `instance_scoped`：带实例、机器、哪台、每台、这些实例、uhost 等上下文的费用/扣费/计费问题。

`isAccountBillingUnsupported` 只对 `account_realtime` 返回 true。原有月度账号汇总逻辑保留，实例词仍只 veto 月度汇总分支，不 veto 余额/流水/发票状态/退款进度/欠费金额这类明确账号财务数据。

混合句规则：只要同一句里包含账号实时财务查询，就整体 hard-block 到财务中心。例如“退款规则是什么，我的退款进度到哪了”“怎么开发票，我的发票状态怎么样”都不拆开回答 FAQ 部分。

实例计费边界：实例路径只能回答已有实例事实、计费方式和扣费原因解释；不承诺查询真实账号账单金额、扣费流水、最高消费实例或可导出的账单明细。

### 2. Planner 提示词收紧

补充 prompt 规则和示例：

- “怎么开发票 / 退款规则 / 欠费后怎么办 / 计费方式区别 / 套餐到期怎么办” -> `knowledge_qa`
- “我的发票状态 / 开票进度 / 发票寄送 / 退款进度 / 退款到账了吗 / 我欠费了吗 / 欠费金额 / 待支付账单 / 账号余额 / 总账单 / 消费流水” -> `billing_account_unsupported`
- “这台为什么扣费 / 哪些停机实例仍可能计费 / 单实例计费方式 / 全实例计费方式汇总” -> `billing_instance`

### 3. FAQ 补全

在 curated FAQ 中补齐最小财务规则集：

- 计费方式区别：按量 / 包时 / 包日 / 包月 / 动态计费
- 欠费处理
- 如何开发票
- 退款规则
- 套餐到期与续费
- 关机 / 停机计费规则补强

FAQ 只写通用规则，不写“我的余额 / 我的发票状态 / 我的退款进度”等需要实时账号数据的内容。

### 4. 检索入口保持现状

现有 `SetKnowledgeRetriever` / `tryStage2BRetrieval` 已经能处理 `knowledge_qa`。本 PR 只补分类、prompt、FAQ 和回归测试，不扩展完整 RAG runtime。

## 测试计划

### Engine hard-block

- “我的发票状态怎么样” -> hard-block，不调用 LLM / tool。
- “退款进度到哪了” -> hard-block，不调用 LLM / tool。
- “我欠费了吗 / 欠费金额多少 / 待支付账单 / 开票进度 / 发票寄送到哪了 / 扣费记录 / 交易记录” -> hard-block，不调用 LLM / tool。
- “怎么开发票，我的发票状态怎么样 / 退款规则是什么，我的退款进度到哪了” -> hard-block，不调用 LLM / tool。
- “账号余额还剩多少 / 本月总账单 / 消费流水” -> 既有 hard-block 保持。
- “怎么开发票 / 退款规则是什么 / 欠费怎么办 / 按量和包日有什么区别 / 套餐到期怎么办” -> 不 hard-block。
- “这台为什么还在扣费 / 哪些停机实例仍可能计费 / 全实例计费方式汇总 / 单实例计费方式” -> 不 hard-block。
- “每台实例的消费流水 / 这些机器导致账号余额还剩多少” -> hard-block，因为仍然要求账号财务中心数据。

### Planner prompt

- 系统 prompt 明确包含三类财务问题的路由规则。
- 系统 prompt 包含“怎么开发票” -> `knowledge_qa` 示例。
- 系统 prompt 包含“我的发票状态” -> `billing_account_unsupported` 示例。

### Knowledge retriever

- “怎么开发票” 命中发票 FAQ。
- “欠费怎么办” 命中欠费 FAQ。
- “按量和包日有什么区别” 命中计费方式 FAQ。
- “退款规则是什么” 命中退款 FAQ。
- “套餐到期怎么办” 命中到期/续费 FAQ。

### Engine retrieval

- planner 输出 `knowledge_qa` 且 retriever 启用时，“怎么开发票”返回 FAQ 内容，不进入通用 ReAct。
- “我的发票状态怎么样”仍被 engine hard-block，优先级高于 planner / retriever。

## 验证命令

```powershell
go test ./internal/engine ./internal/intent ./internal/knowledge -count=1
go test ./... -count=1
python scripts/test_planner_vs_guard_diff.py
git diff --check
.\scripts\secret_scan.ps1
```
