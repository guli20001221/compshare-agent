---
last_updated: 2026-04-13T21:00+08:00
---
# Project Context
## 当前状态
W2 开发完成：FAQ 常驻 Context + 结构化知识 Tool（GPU 规格查询/场景推荐）已集成到 ReAct 引擎。33 个测试全部通过，二进制编译成功。

## 进行中
- [ ] W2 端到端验证 — 需启动 CLI 实际测试 "4090 和 A100 有什么区别" / "关机后还扣费吗"

## 最近完成
- [x] W1 — LLM 客户端 + CLI + ReAct + ExternalExecutor + 3 Tool + 安全分级 + L1 确认
- [x] W2 FAQ 常驻 — 飞书 FAQ 抓取（firecrawl）→ 整理为 docs/faq/ → 压缩为 ~1600 token QA 对嵌入 System Prompt
- [x] W2 结构化知识 — internal/knowledge/ 包：16 种 GPU 规格表（VRAM/FP16/MaxGPU 等，从 gpu.go 同步）+ 6 种场景推荐矩阵
- [x] W2 Tool 注册 — GetGPUSpecs + GetGPURecommendation 注册到 registry，engine 本地执行（不走 API）
- [x] W2 测试 — knowledge 包 15 个测试 + prompt/faq 8 个测试，全部通过

## 架构决策
| 决策 | 选择 | 原因 | 日期 |
|------|------|------|------|
| FAQ 存储方式 | Go 常量嵌入 System Prompt | ~1600 token 可接受，免除向量库依赖 | 2026-04-13 |
| 知识 Tool 路由 | engine.go 中 IsKnowledgeTool 分流 | 本地执行无需 API 调用和安全检查 | 2026-04-13 |
| GPU 规格来源 | gpu.go 同步 MaxCPU/MaxMemory + 公开 VRAM/FP16 | 价格/库存仍走实时 API | 2026-04-13 |

## 已知问题
- [W1遗留] 对话历史无上限（需滑动窗口/token 计数）— 中
- [W1遗留] engine/llm 包无测试覆盖 — 低
- [W2] GPU 价格硬编码风险 — 已规避（价格走实时 API，规格表只含物理参数）
