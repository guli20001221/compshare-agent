---
last_updated: 2026-05-07T18:30:48+08:00
---
# Project Context

## 当前状态
Stage 2A Phase 0 设计基线已在 `origin/main`，T-002 SecretBoundary 和 T-003 CapabilityRegistry 已完成并进入 review/merge 流程。当前工作线为 T-004a EntityRegistry parser/resolver，分支 `codex/stage2a-t004a-entity-registry`，目标是在不接入 engine 主流程的前提下提供账号实例快照、ID/名称解析和基础过滤能力，供后续 T-007a IntentPlan validator 使用。

## 进行中
- [ ] T-004a EntityRegistry parser/resolver — 已实现 `internal/entity` 纯逻辑模块和 gated 真实账号 integration；下一步是本地提交并交给 Claude review。

## 最近完成
- [x] T-003 CapabilityRegistry — `internal/llm/capability.go` 编码 Modelverse/Qwen/Doubao/GLM 能力矩阵；记录 2026-05-07 Doubao Lite probe 与 2026-05-01 早期 400 反例；验证 `go test ./... -count=1` PASS。
- [x] T-002 SecretBoundary — 占位符 YAML、env 注入、secret scan、setup 文档和 RedactForLLM/Trace primitives；runtime wire-up 明确留给 T-001/T-006。
- [x] Stage 2A baseline docs — `docs/agent/plan/stage2-intent-planner.md` 与 `stage2a-phase0-tickets.md` 已 freeze 并推到 `origin/main`。
- [x] Stage 1.5 Plan A archive — `archive/stage1.5-plan-a` 保存 resource-info guard 实证实现，作为后续迁移参考。

## 架构决策
| 决策 | 选择 | 原因 | 日期 |
| --- | --- | --- | --- |
| Stage 2A 形态 | workflow with LLM steps | 资源/监控/计费事实必须由 deterministic handler 和 API grounding 控制 | 2026-05-07 |
| SecretBoundary runtime 接入 | T-002 只提供函数，T-001/T-006 接入 | 避免和现有 sanitizer / dual-channel token display 产生双重脱敏副作用 | 2026-05-07 |
| CapabilityRegistry 范围 | 先覆盖国产/Modelverse 常用模型，国外模型暂不进入默认矩阵 | 当前验收关注国产模型权限与 thinking-mode/tool_choice 兼容性 | 2026-05-07 |
| EntityRegistry T-004a 范围 | 纯 parser/resolver，不影响 engine 主流程 | Phase 0 先为 T-007a validator 提供稳定实体地面真值，不提前切流 | 2026-05-07 |
| EntityResolver 名称匹配 | 精确归一化优先，模糊匹配返回 AMBIGUOUS；候选排序稳定 | 防止 LLM 自由补 ID，同时把模糊用户表达留给上层澄清 | 2026-05-07 |

## 已知问题
- [high] T-001 SafeToolExecutor 尚未实现，现有 force-tool guards 和 sanitizer 仍在老 engine 路径中。
- [medium] T-003 仍有 follow-up：capability override 文件失败时 stderr log、默认端口归一化、后续并行 tool-call 能力位。
- [medium] EntityRegistry Phase 0 仅拉 `DescribeCompShareInstance Limit=100` 单页，账号实例数超过 100 时只设置 `Truncated`；分页留到 Phase 1 切流前。
- [medium] 真实账号 integration 依赖本地 `.env.local` 注入，不能在普通 CI 默认开启。
