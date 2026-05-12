---
last_updated: 2026-05-12T17:40:00+08:00
---
# Project Context
## 当前状态
当前分支 `codex/pre-rag-trace-closure` 位于 `F:\compshare-agent-worktrees\pre-rag-trace-closure`，基于 PR #65 合并后的 `origin/main`。PR-C trace 收口已实现并通过本地验证，待提交/开 PR。

## 进行中
- [x] PR-C trace 收口 — trace 输出瘦身、真实 token 统计、启动清理接入、trace 脱敏补强。

## 最近完成
- [x] trace.v0.2 输出瘦身 — 通过 marshal-time projection 省略空的 runtime/planner/renderer/retrieval/outcome 等块。
- [x] token 统计 — LLM streaming usage、planner usage、grounded renderer usage 汇总到 `outcome.total_tokens`；没有 provider usage 时不写 token 字段。
- [x] trace 清理 — CLI 启动时调用 30 天 retention cleanup，失败只告警不阻塞。
- [x] trace 脱敏 — OnBeforeCall 先做参数脱敏；SafeToolExecutor 的 TraceResult 与 LLMResult 使用同一套 result redaction。
- [x] 子 agent review — plan review 和 code review 均完成；阻塞项是 outcome 误写 0，已修复并复审通过。

## 架构决策
| 决策 | 选择 | 原因 | 日期 |
| --- | --- | --- | --- |
| trace 空块处理 | 使用 MarshalJSON 投影，不改变读模型 | 兼容旧 trace，同时让新 trace 更干净 | 2026-05-12 |
| token 统计 | 只记录 provider 明确返回的 usage；不估算 | 避免把猜测数字写进审计数据 | 2026-05-12 |
| hallucination 指标 | 当前不输出 0；等检测器存在后再填 | 不为 dashboard 造假数据 | 2026-05-12 |
| include_usage fallback | 只在 provider 明确不支持该字段时重试 | 避免吞掉其他真实 400 错误 | 2026-05-12 |

## 已知问题
- [medium] 完整 RAG 语料仍未接入，后续从 GitLab/飞书知识库开始 Stage 2B P0-P4。
- [low] trace.v0.2 仍只记录汇总 token，不拆分每个 LLM 调用的 token 明细。
