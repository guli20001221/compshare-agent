---
last_updated: 2026-05-08T17:30:00+08:00
---
# Project Context

## 当前状态
Stage 2A Phase 0 已完成并合并 T-002 SecretBoundary、T-003 CapabilityRegistry、T-004a EntityRegistry、T-007a offline IntentPlan、T-001 SafeToolExecutor、T-001.f1 monitor recall、T-001.f2 account billing hard-block、T-003.f1 ds v4 flash tool-choice decision。当前进行中是 PR #12：ds v4 flash monitor stale-reuse probe（read-only artifact）。

## 进行中
- [ ] PR #12 monitor stale-reuse probe — `eval/capability/2026-05-08-ds-v4-flash-monitor-stale-reuse-probe.md` 证明 ds v4 flash auto routing 在 2/3 adjacent monitor follow-up 场景复用上一轮监控数值；待 review/merge 后决定非 object-tool-choice mitigation。

## 最近完成
- [x] PR #11 T-003.f1 Step 3 — merged `b146305`，标记 ds v4 flash `SupportsObjectToolChoice=false` + `IsThinkingMode=true`，删除 `shouldForceBillingDiagnosis`，monitor recall 改为 capability-gated。
- [x] PR #10 ds v4 flash tool-choice probe — merged `59a4344`，production request shape 下 object tool_choice 0/6 PASS，required/auto 5/6 PASS。
- [x] PR #8 T-001.f2 account billing hard-block — merged `5d54230`，账号级月度账单/余额/消费流水 hard-block 为永久产品边界。
- [x] PR #6 T-001 SafeToolExecutor — merged `ef94aca`，tool execution security/filter/sanitize/retry/monitor guard 迁入 SafeToolExecutor。
- [x] PR #5 T-007a offline IntentPlan — merged，提供 intent schema/validator/offline fixtures。
- [x] PR #2/#3/#4 — SecretBoundary / CapabilityRegistry / EntityRegistry 基础模块已合并。

## 架构决策
| 决策 | 选择 | 原因 | 日期 |
| --- | --- | --- | --- |
| Stage 2A 基准模型 | `deepseek-v4-flash` | 后续 force-tool / response_format / thinking-mode 工单必须优先验证生产 request shape | 2026-05-08 |
| ds v4 flash object tool_choice | 标记 unsupported | PR #10 生产形态 probe 0/6 PASS，稳定 400 | 2026-05-08 |
| Billing follow-up guard | 删除 `shouldForceBillingDiagnosis` | auto routing 与 required 同为 5/6 PASS，object force 会 400，强制路由无增益 | 2026-05-08 |
| Monitor follow-up guard | 保留但 capability-gated | stale-reuse probe 证明 auto 不可靠，但 ds v4 flash 不能用 object tool_choice | 2026-05-08 |
| Account billing hard-block | 永久保留 | 账号级财务数据是产品能力边界，不随 IntentPlan 切流删除 | 2026-05-08 |

## 已知问题
- [high] ds v4 flash monitor follow-up auto routing 存在 stale-reuse：2/3 adjacent follow-up 未重新调用 `GetCompShareInstanceMonitor` 却输出上一轮数值。下一步需要设计窄动态 system/developer nudge，再重跑 PR #12 probe。
- [medium] Billing stale follow-up auto probe 还未做；`shouldForceBillingDiagnosis` 已删除，需事后验证 auto 在真实续问场景是否足够。
- [medium] T-005 RateLimiter、T-006 Trace skeleton、T-004b EntityRegistry refresh/mutex 仍未开工。
- [medium] T-003 follow-up：capability override 文件 silent fail log、默认端口归一化待补。
