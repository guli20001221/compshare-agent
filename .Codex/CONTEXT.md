---
last_updated: 2026-05-16T23:50+08:00
---
# Project Context

## 当前状态
当前分支 `codex/pr-rag-11a-1` 位于 `F:\compshare-agent-worktrees\rag-11a-1`。RAG-11a.1 Phase 2 已完成：在 #87 schema 合并后的 main 上扩入第一批 21 篇净新官方文档，4 篇已覆盖文档保持跳过；正式知识库从 62 条扩到 121 条，评估报告 PASS，等待提交、推送和开 PR。

## 进行中
- [x] RAG-11a.1 Phase 2 — 已完成图片说明、chunk 生成、评估、部署文件和 digest 更新；下一步是提交并创建 PR。

## 最近完成
- [x] PR #87 schema 合并 — `KBChunk.source_origin` / `surface_url` 已落 main，digest 机制保持 LF-normalized。
- [x] RAG-11a.1 语料生成 — 21 篇 active 文档生成 59 条新 chunk；4 篇 skipped 文档未重复生成。
- [x] RAG-11a.1 评估 — 269 题，anchor 15/15，Top-3 79.8%，引用 100%，安全泄漏 0，编造 0，忠实度 98.4%。
- [x] 安全规则修正 — 公开 API 文档里的 `Bearer api_key` / `Bearer <YOUR_API_KEY>` 等占位写法不再被误判为密钥；真实 `Bearer abc` / `api_key=abc` 仍会拦截。

## 架构决策
| 决策 | 选择 | 原因 | 日期 |
| --- | --- | --- | --- |
| RAG-11a.1 skipped 文档 | `package` / `logic` / `bill` / `billdescribe` 不重复入库 | 这 4 篇已在 bootstrap 14 的旧 62 条里覆盖，避免重复 chunk | 2026-05-16 |
| RAG-11a.1 kb_version | `kb.stage2b.w0.2026-05-16.rag-11a-1` | 标识本次语料扩容后的可部署快照 | 2026-05-16 |
| API 文档占位密钥 | 允许明确占位写法，继续拦真实密钥形态 | 防止 ModelVerse 接入文档被安全规则误杀，同时保留泄漏门槛 | 2026-05-16 |

## 已知问题
- [low] `eval_report.md` 中 4 个 judge_flagged 都是安全拒答类提示，不阻塞 gate；后续可优化 judge 对拒答的解释口径。
- [medium] RAG-11a.2 的后续批次仍需等用户审定清单后再启动。
