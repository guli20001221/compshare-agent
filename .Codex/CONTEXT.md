---
last_updated: 2026-05-07T00:00+08:00
---
# Project Context

## 当前状态
Stage 2A Phase 0 正在推进。T-002 SecretBoundary 已在 PR #2 中完成并获得 review 通过。当前分支 `codex/stage2a-t003-capability-registry` 从 `origin/main` 创建，独立实现 T-003 CapabilityRegistry，不依赖 T-002 代码。

## 进行中
- [ ] T-003 CapabilityRegistry — 已新增 `internal/llm/capability.go`、测试、testdata fixture 和 setup 文档；待 review。

## 最近完成
- [x] T-002 SecretBoundary — PR #2，完成 YAML 占位符化、SecretBoundary 原语、本地 env 注入、secret scan 和 setup 文档。
- [x] Stage 2A baseline 文档 — `docs/agent/plan/stage2-intent-planner.md` 已提交到 `origin/main`。
- [x] Phase 0 工单清单 — `docs/agent/plan/stage2a-phase0-tickets.md` 已提交到 `origin/main`。
- [x] Plan A 参考实现归档 — `archive/stage1.5-plan-a` 已推到远端。

## 架构决策
| 决策 | 选择 | 原因 | 日期 |
| --- | --- | --- | --- |
| T-003 范围 | 只交付 lookup registry，不接 planner/engine | 保持 Phase 0 最小切片，后续 T-007a/T-001 消费能力表 | 2026-05-07 |
| Capability key | `(normalized base_url, normalized model)` | 明确 provider/model 组合差异，覆盖 Modelverse/Doubao thinking-mode 等实证问题 | 2026-05-07 |
| Unknown capability | 全部结构化输出/tool-choice 能力保守 false | 避免新 provider 默认走可能 400 的能力路径 | 2026-05-07 |
| Override | `COMPSHARE_LLM_CAPABILITY_FILE` YAML，每次 lookup 读取 | 支持开发期热更新，不需重新编译 | 2026-05-07 |

## 已知问题
- [medium] T-003 尚未接入 planner 或 engine；真正消费能力表在 T-007a/T-001。
- [medium] Modelverse/Qwen3.6 thinking 行为来自历史观测和保守假设，后续真实 E2E 可通过 YAML override 调整。
