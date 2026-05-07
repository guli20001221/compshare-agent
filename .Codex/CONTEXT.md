---
last_updated: 2026-05-07T00:00+08:00
---
# Project Context

## 当前状态
`main` 已冻结 Stage 2A intent planner baseline 和 Phase 0 工单清单；Plan A 实证实现保存在 `archive/stage1.5-plan-a`。当前工作在独立 worktree/分支 `codex/stage2a-t002-secret-boundary` 上推进 Phase 0 T-002 SecretBoundary，不触碰主工作区的历史本地改动。

## 进行中
- [ ] T-002 SecretBoundary — 已实现占位符配置加载、LLM/trace 脱敏函数、示例配置占位符化、eval `.env.example`、本地 `.env.local` 加载脚本、版本化 pre-commit secret scan、setup 文档；真实账号 E2E 已通过，待人工 review。

## 最近完成
- [x] Stage 2A baseline 文档 — `docs/agent/plan/stage2-intent-planner.md` 已提交到 `origin/main`。
- [x] Phase 0 工单清单 — `docs/agent/plan/stage2a-phase0-tickets.md` 已提交到 `origin/main`。
- [x] Plan A 参考实现归档 — `archive/stage1.5-plan-a` 已推到远端。

## 架构决策
| 决策 | 选择 | 原因 | 日期 |
| --- | --- | --- | --- |
| Stage 2A 形态 | workflow with LLM steps | 优先可预测、可审计、可回滚，不回退自由 ReAct | 2026-05-07 |
| SecretBoundary config | YAML 只接受 `${ENV_VAR}` 占位符，真实密钥只从环境变量读取 | 防止 eval/deploy 配置将真实 key 带入 git、trace 或 LLM 上下文 | 2026-05-07 |
| 本地真实账号 E2E key | 允许放在 gitignored `.env.local`，通过 `scripts/load_env.ps1` 注入进当前进程环境变量 | 保留“config 不含明文 secret”的契约，同时方便手工 E2E | 2026-05-07 |
| Trace 脱敏 | secret redaction + IP mask + billing/cost SHA-256 短 hash | trace 可关联问题但不暴露账号财务/IP 明细 | 2026-05-07 |
| ProjectId | 可空；显式配置时也必须走占位符 | 兼容启动期自动发现，同时避免账号级常量写入 YAML | 2026-05-07 |

## 已知问题
- [medium] `.githooks/pre-commit` 已版本化，但本地启用仍需执行 `git config core.hooksPath .githooks`。
- [low] `scripts/secret_scan.ps1` 是轻量防线，不替代后续接入 truffleHog/git-secrets 的完整扫描。
