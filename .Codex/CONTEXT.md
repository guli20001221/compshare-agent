---
last_updated: 2026-05-08T01:00:00+08:00
---
# Project Context

## 当前状态
Stage 2A Phase 0 三个基础模块（SecretBoundary / CapabilityRegistry / EntityRegistry）已 merge 到 `origin/main`（HEAD `2c1497f`）。T-007a IntentPlan stack 正在 rebase 到 main 的收尾阶段，待完成 rebase、验证并开 PR #5。Phase 0 主流程仍走老 ReAct + 老 guard，T-001 SafeToolExecutor 尚未开工。

## 进行中
- [ ] T-007a IntentPlan / Validator / Offline planner — rebase 到 `origin/main` 正在收尾，待 `rebase --continue` 后验证并开 PR #5。

## 最近完成
- [x] PR #4 T-004a EntityRegistry — merged at `2c1497f`，提供 `internal/entity` 纯 parser/resolver。
- [x] PR #3 T-003 CapabilityRegistry — merged at `6129ab0`，提供 `internal/llm` capability matrix。
- [x] PR #2 T-002 SecretBoundary — merged at `ff87515`，提供占位符 YAML / secret scan / RedactForLLM/Trace。
- [x] T-007a 本地实现 — `internal/intent` types/validator/planner + 64 fixtures，offline eval `legal_rate=1.00 target_accuracy=1.00 unknown_accuracy=1.00`。
- [x] Stage 2A baseline docs — `docs/agent/plan/stage2-intent-planner.md` + `stage2a-phase0-tickets.md` 已 freeze。

## 架构决策
| 决策 | 选择 | 原因 | 日期 |
| --- | --- | --- | --- |
| Stage 2A 形态 | workflow with LLM steps | 资源/监控/计费事实必须由 deterministic handler 控制 | 2026-05-07 |
| EntityRegistry T-004a 范围 | 纯 parser/resolver，不接 engine 主流程 | Phase 0 先为 validator 提供地面真值 | 2026-05-07 |
| NameIndex 暴露 | 公开字段 + godoc 明确 normalized key | 最小 API + 降低误用 | 2026-05-07 |
| T-007a 范围 | types + validator + planner + offline fixtures | Phase 0 不让 planner 影响主流程 | 2026-05-07 |
| Snapshot 字段扩展 | T-007a 起手只补 StartTime/ImageType | MonitorMessages 暂无 populator | 2026-05-07 |
| Name slot 规则 | validator 拒长度 < 2 的 name slot | 避免短 query 触发大批 AMBIGUOUS | 2026-05-07 |
| Planner fallback | 失败返回 unknown，不执行 API/RAG | 符合 Stage 2A invalid-plan 固定话术 | 2026-05-07 |

## 已知问题
- [high] T-001 SafeToolExecutor 尚未实现，老 force-tool guards 仍在 engine 路径中。
- [medium] T-002 follow-up：F1 raw bearer / F2 password field / F3 OAuth tokens 待 T-001 wire-up 一并修。
- [medium] T-003 follow-up：override 文件 silent fail 应 stderr log、默认端口归一化。
- [medium] EntityRegistry Phase 0 仅拉 Limit=100 单页，>100 实例账号只设 Truncated；分页 + warn 留 T-004b。
- [medium] T-004b 待补 mutex / async refresh / SafeToolExecutor 接入。
- [medium] T-007a offline eval 用 deterministic heuristic LLM；真 LLM shadow 留 T-007b。
