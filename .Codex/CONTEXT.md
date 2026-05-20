---
last_updated: 2026-05-20T15:22:11+08:00
---
# Project Context
## 当前状态
当前分支 `codex/intent-routing-contracts` 位于隔离 worktree，已完成意图路由 1-5 项调整：工具白名单、离线样例、价格边界、诊断追问、知识库硬兜底限定。已通过模块测试、全仓测试、两组真实 CLI mock 烟测和子 agent 独立 review，等待提交推送开 PR。

## 进行中
- [ ] intent-routing-contracts PR — 已完成实现、验证和复核 / 下一步提交推送开 PR

## 最近完成
- [x] 意图到工具白名单 — `validator.go` 复用 capability registry，阻止 intent 使用越界工具。
- [x] 离线样例修正 — diagnosis/vague_failure 样例跟随新边界，修复 planner prompt 用户问题提取。
- [x] 价格边界 — 直接价格问题回到普通工具链，计费规则类问题仍保留知识库。
- [x] 诊断追问 — 仅在诊断无目标且多实例、或模糊故障时追问，不进入主循环编造。
- [x] 知识库硬兜底收窄 — citation gate 仅作用于知识库路径，并补双轮污染回归测试。
- [x] P1 review 修复 — 诊断追问必须显式开启 `diagnosis` / `vague_failure`，不会被 resource/monitor/RAG 分诊顺带打开。

## 架构决策
| 决策 | 选择 | 原因 | 日期 |
| --- | --- | --- | --- |
| 工具权限 | 在 validator 层校验 intent -> required_tools 白名单 | 先建立结构性契约，降低后续 prompt/skill 漂移风险 | 2026-05-20 |
| 价格边界 | 运行时价格问题返回 unknown，交给普通工具链 | 价格是实时数据，不应进入知识库静态回答 | 2026-05-20 |
| 诊断追问 | 在 planner dispatch 后、主循环前做窄条件追问 | 避免模糊诊断进入无依据回答，同时不扩大 mainloop 复杂度 | 2026-05-20 |
| 诊断开关 | `diagnosis` / `vague_failure` 必须显式加入 `USE_INTENT_PLANNER_FOR` | 防止资源、监控或知识库分诊开启时顺带打断原有诊断流程 | 2026-05-20 |
| cited contract | 保留 hard gate，但用本轮 knowledge-path 标记限定 | 保住知识库答案必须有依据的硬约束，避免误伤非知识库问题 | 2026-05-20 |

## 已知问题
- [low] 真实线上模型分类表现仍需后续 trace 数据观察；本 PR 主要用提示词、fixtures、脚本化单测和 CLI mock 烟测锁定行为。
