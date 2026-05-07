---
last_updated: 2026-05-08T00:38:00+08:00
---
# Project Context

## 当前状态
PR #2 T-002 SecretBoundary 和 PR #3 T-003 CapabilityRegistry 已合入 `origin/main`。当前分支 `codex/stage2a-t004a-entity-registry` 正在同步 main 后继续承载 T-004a EntityRegistry，用于合并 PR #4。T-004a 仍保持 Phase 0 最小范围：纯 parser/resolver，不接 engine 主流程。

## 进行中
- [ ] PR #4 T-004a EntityRegistry — 已同步 PR #2/#3 后的 main，解决 `.Codex/CONTEXT.md` checkpoint 冲突；下一步验证并 merge。

## 最近完成
- [x] PR #3 T-003 CapabilityRegistry — 已 merge 到 main；提供 `internal/llm` capability matrix 与 override 文件支持。
- [x] PR #2 T-002 SecretBoundary — 已 merge 到 main；提供占位符 YAML、env 注入、secret scan、setup 文档和 RedactForLLM/Trace primitives。
- [x] T-004a EntityRegistry — 新增 `internal/entity` 纯 parser/resolver、ResolveByID/ResolveByName/Filter、gated real-account integration；已 review APPROVE。
- [x] Stage 2A baseline docs — `docs/agent/plan/stage2-intent-planner.md` 与 `stage2a-phase0-tickets.md` 已 freeze 并推到 `origin/main`。

## 架构决策
| 决策 | 选择 | 原因 | 日期 |
| --- | --- | --- | --- |
| EntityRegistry T-004a 范围 | 纯 parser/resolver，不影响 engine 主流程 | Phase 0 先为 T-007a validator 提供稳定实体地面真值，不提前切流 | 2026-05-07 |
| EntityResolver 名称匹配 | 精确归一化优先，模糊匹配返回 AMBIGUOUS；候选排序稳定 | 防止 LLM 自由补 ID，同时把模糊用户表达留给上层澄清 | 2026-05-07 |
| NameIndex 暴露 | 公开字段但 godoc 明确 normalized key，调用方优先用 ResolveByName | 保持 T-004a 最小 API，同时降低下游误用概率 | 2026-05-07 |
| Integration region | `COMPSHARE_REGION` 可覆盖，默认 `cn-wlcb` | 支持其他区域账号开发者跑 gated integration | 2026-05-07 |

## 已知问题
- [high] T-001 SafeToolExecutor 尚未实现，现有 force-tool guards 和 sanitizer 仍在老 engine 路径中。
- [medium] EntityRegistry Phase 0 仅拉 `DescribeCompShareInstance Limit=100` 单页，账号实例数超过 100 时只设置 `Truncated`；分页与 truncated warning 留到后续切流前。
- [medium] T-004b 需要补 mutex/async refresh 与 SafeToolExecutor 接入。
- [medium] T-007a 已在 stacked branch 本地完成，等待 PR #4 merge 后 rebase 到 main。
