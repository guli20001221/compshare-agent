---
last_updated: 2026-05-12T02:20:00+08:00
---
# Project Context

## 当前状态
`origin/main` 已到 `fb1fd1b`（PR #61 resource query structured filter / factual envelope / grounded renderer hardening 已合并）。当前分支 `codex/phase1-resource-selection-clarification` 在 `F:\compshare-agent-worktrees\resource-selection-clarification`，实现资源选择/澄清层：监控类问题缺少唯一实例时先列候选，用户回复数字、实例 ID 或完整名称后继续查当前监控。

## 进行中
- [ ] PR: Phase 1 resource selection clarification — implementation complete, subagent review APPROVE, verification PASS，待 push/open PR。

## 最近完成
- [x] Resource query filter PR #61 — merged `fb1fd1b`，修复 running/stopped/gpu/AND filters 与 grounded renderer 计数/ID。
- [x] Resource selection plan — `docs/plans/2026-05-12-resource-selection-clarification.md`。
- [x] Resource selection helpers — pending state / matching / prompt rendering / sanitization。
- [x] Monitor candidate selection — `selection_required`，候选 refresh 失败不回落 ReAct。
- [x] Selection continuation — 数字 / UHostId / 完整名称续接原 monitor plan，同一 registry snapshot。
- [x] CLI startup suggestion fix — 首页推荐编号只在第一轮生效，避免和选择数字冲突。
- [x] Real-account smoke — `eval/shadow_qa/2026-05-12-resource-selection-smoke/README.md`，ds v4 flash 验证选择链路 + resource_info 回归。

## 架构决策
| 决策 | 选择 | 原因 | 日期 |
| --- | --- | --- | --- |
| 资源选择层位置 | Engine pending state，位于 hard-block 后、planner/ReAct 前 | 不让模型猜实例；同时保留 account hard-block 优先级 | 2026-05-12 |
| 选择输入 | 支持数字、UHostId、完整实例名；重复名要求更具体 | 贴近控制台选择资源体验，避免误选 | 2026-05-12 |
| 选择续接 | 复用原 IntentPlan + 同一 RegistrySnapshot，只替换目标 | 保证用户看到的候选和后续查询一致 | 2026-05-12 |
| 性能问题分类 | CPU/GPU/内存/显存高低/空闲先走 monitor_query | “CPU 高怎么办”第一步应查监控，而不是直接诊断/自由回答 | 2026-05-12 |
| CLI 数字推荐 | 启动推荐编号仅第一轮生效 | 后续数字要留给资源选择等对话动作 | 2026-05-12 |

## 已知问题
- [medium] 选择候选目前面向 `monitor_query` 单实例澄清；资源查询仍按 #61 结构化筛选，不做多实例选择。
- [medium] `CPU 高怎么办` 现在可进入监控选择，但后续“诊断建议”仍只基于当前监控事实总结；不做 SSH/远程命令/实例内部排查。
- [medium] 历史监控、图表、全账户异常扫描、IP 反查、RAG/FAQ 均不在本 slice。
