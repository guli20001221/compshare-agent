---
last_updated: 2026-05-08T00:33:00+08:00
---
# Project Context

## 当前状态
PR #2 T-002 SecretBoundary 已合入 `origin/main`。当前分支 `codex/stage2a-t003-capability-registry` 正在同步 main 后继续承载 T-003 CapabilityRegistry，用于合并 PR #3。T-003 仍保持 Phase 0 最小范围：只提供 LLM capability lookup/override，不接 planner 或 engine 主流程。

## 进行中
- [ ] PR #3 T-003 CapabilityRegistry — 已同步 PR #2 后的 main，解决 `.Codex/CONTEXT.md` checkpoint 冲突；下一步验证并 merge。

## 最近完成
- [x] PR #2 T-002 SecretBoundary — 已 merge 到 main；提供占位符 YAML、env 注入、secret scan、setup 文档和 RedactForLLM/Trace primitives。
- [x] T-003 CapabilityRegistry — 新增 `internal/llm/capability.go`、测试、testdata fixture 和 setup 文档；已 review APPROVE。
- [x] Stage 2A baseline docs — `docs/agent/plan/stage2-intent-planner.md` 与 `stage2a-phase0-tickets.md` 已 freeze 并推到 `origin/main`。

## 架构决策
| 决策 | 选择 | 原因 | 日期 |
| --- | --- | --- | --- |
| T-003 范围 | 只交付 lookup registry，不接 planner/engine | 保持 Phase 0 最小切片，后续 T-007a/T-001 消费能力表 | 2026-05-07 |
| Capability key | `(normalized base_url, normalized model)` | 明确 provider/model 组合差异，覆盖 Modelverse/Doubao thinking-mode 等实证问题 | 2026-05-07 |
| Unknown capability | 全部结构化输出和 tool-choice 能力保守 false | 避免新 provider 默认走可能 400 的能力路径 | 2026-05-07 |
| Override | `COMPSHARE_LLM_CAPABILITY_FILE` YAML，每次 lookup 读取 | 支持开发期热更新，不需重新编译 | 2026-05-07 |
| SecretBoundary runtime 接入 | T-002 只提供函数，T-001/T-006 接入 | 避免和现有 sanitizer / dual-channel token display 产生双重脱敏副作用 | 2026-05-07 |

## 已知问题
- [medium] T-003 尚未接入 planner 或 engine；真正消费能力表在 T-007a/T-001。
- [medium] CapabilityRegistry follow-up：override 文件失败时 stderr log、默认端口归一化、后续并行 tool-call 能力位。
- [medium] T-002 follow-up：secret scan raw bearer/password/OAuth 命名扩展，留后续小修。
