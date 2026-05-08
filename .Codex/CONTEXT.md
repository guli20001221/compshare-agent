---
last_updated: 2026-05-08T18:20:00+08:00
---
# Project Context

## 当前状态
Stage 2A Phase 0 已合并 T-002 SecretBoundary、T-003 CapabilityRegistry、T-004a EntityRegistry、T-007a offline IntentPlan、T-001 SafeToolExecutor、T-001.f1 monitor recall、T-001.f2 account billing hard-block、T-003.f1 ds v4 flash tool-choice decision，以及 PR #12 monitor stale-reuse probe。当前进行中是 T-007b monitor acceptance 文档 PR：把 PR #12 的 stale-reuse 证据转成 shadow trace / Phase 1 monitor handler promotion gate。

## 进行中
- [ ] T-007b monitor acceptance doc — `docs/agent/plan/stage2a-t007b-monitor-acceptance.md`，要求 PR #12 的 3 个 monitor follow-up case 进入 T-007b trace 与 Phase 1 handler 验收，覆盖 mixed monitor+billing/diagnosis/operation 边界。

## 最近完成
- [x] PR #12 monitor stale-reuse probe — merged `d9e0ee6`，证明 ds v4 flash auto routing 在 2/3 adjacent monitor follow-up 场景复用上一轮监控数值。
- [x] PR #11 T-003.f1 Step 3 — merged `b146305`，标记 ds v4 flash `SupportsObjectToolChoice=false` + `IsThinkingMode=true`，删除 `shouldForceBillingDiagnosis`，monitor recall 改为 capability-gated。
- [x] PR #10 ds v4 flash tool-choice probe — merged `59a4344`，production request shape 下 object tool_choice 0/6 PASS，required/auto 5/6 PASS。
- [x] PR #8 T-001.f2 account billing hard-block — merged `5d54230`，账号级月度账单/余额/消费流水 hard-block 为永久产品边界。
- [x] PR #6 T-001 SafeToolExecutor — merged `ef94aca`，tool execution security/filter/sanitize/retry/monitor guard 迁入 SafeToolExecutor。
- [x] PR #5 T-007a offline IntentPlan — merged，提供 intent schema/validator/offline fixtures。

## 架构决策
| 决策 | 选择 | 原因 | 日期 |
| --- | --- | --- | --- |
| T-007b 形态 | shadow planner + trace + dashboard，不改主流程 | Phase 0 只观测，不切业务意图 | 2026-05-08 |
| Monitor freshness promotion gate | `monitor_query` / `monitor_history` handler 当前 turn 必须调用 `GetCompShareInstanceMonitor` | PR #12 证明 auto routing 会复用上一轮监控数值 | 2026-05-08 |
| Mixed monitor scope | monitor freshness 不覆盖 account hard-block / diagnosis / operation 边界 | 避免单一 monitor handler 抢占更高优先级或混合意图 | 2026-05-08 |
| `shouldForceMonitorRecall` 生命周期 | 保留到 planner-driven monitor handler 通过 promotion gate 后再删 | 对支持 object tool_choice 的模型仍有效；ds v4 flash 走 capability-gated auto | 2026-05-08 |
| Account billing hard-block | 永久保留 | 账号级财务数据是产品能力边界，不随 IntentPlan 切流删除 | 2026-05-08 |

## 已知问题
- [high] ds v4 flash monitor follow-up auto routing 存在 stale-reuse：2/3 adjacent follow-up 未重新调用 `GetCompShareInstanceMonitor` 却输出上一轮数值。已通过 T-007b acceptance 文档纳入后续验收。
- [medium] Billing stale follow-up auto probe 还未做；`shouldForceBillingDiagnosis` 已删除，需事后验证 auto 在真实续问场景是否足够。
- [medium] T-005 RateLimiter、T-006 Trace skeleton、T-004b EntityRegistry refresh/mutex 仍未开工。
- [medium] T-003 follow-up：capability override 文件 silent fail log、默认端口归一化待补。
