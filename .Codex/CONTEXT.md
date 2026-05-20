---
last_updated: 2026-05-20T20:35+08:00
---
# Project Context

## 当前状态
主线已进入 post-#113 阶段。今天完成了诊断类工具优化和 Prompt P1-P4 重构：静态 FAQ 已下线；RAG 系统提示拆成共享片段，Go 运行时和 Python eval 读取同一份模板；capability planner 片段改由 markdown frontmatter 生成；planner examples 已按 intent 分组并带来源说明。P2/P3/P4 均已通过子 agent 只读复审；全量 `go test ./... -count=1`、RAG Python 测试和临时目录构建均通过。

## 进行中
- [ ] #50 Lane B — 决策为 defer，qwen3_full 保持 opt-in；待 #53 cc02 corpus 修复后重新评估。
- [ ] #53 cc02 corpus disambig — P1，需改写无卡模式概念澄清后重跑 focused eval。
- [ ] #60 pa08 planner — P1，planner `intent=unknown / fallback_ineligible` 导致未进 RAG，需要单独修。

## 最近完成
- [x] 诊断类工具优化 — SSH 改用 `SshLoginCommand`；端口诊断中 SSH 走实例登录入口；GPU/SSH 监控缺失时不再说成正常；镜像诊断不再把 Running 说成“镜像加载正常”；普通模式和只读模式提示词都加入“只读自查/可选修复”边界。
- [x] 诊断工具验证 — `go test ./internal/diagnosis ./internal/engine ./internal/tools ./internal/prompt ./cmd`、`go test ./internal/intent ./internal/renderer`、`go test ./...`、`go build -o agent.exe ./cmd` 均通过。
- [x] 静态 FAQ 下线 — 删除 `FAQContent/ReadOnlyFAQContent` 注入，ReAct 主提示只保留知识来源边界；初始化和 prompt 单测加入旧 FAQ 反向断言。
- [x] 静态 FAQ 验证 — `go test ./internal/prompt`、`go test ./internal/engine`、`go test ./internal/prompt ./internal/engine ./internal/intent ./internal/renderer`、`go test ./... -count=1` 均通过。
- [x] Prompt P2-P4 重构 — RAG 提示拆为 `rag_system_segments/*.txt`；capability prompt 片段由 `internal/intent/capabilities/*.md` frontmatter 生成；planner examples 按 intent 分组并补逐条来源。
- [x] Prompt 子 agent review — P1/P2/P3/P4 复审报告在 `.Codex/artifacts/p1-static-faq-removal-review-2026-05-20.md`、`p2-rag-prompt-review-2026-05-20.md`、`p3-capability-frontmatter-review-2026-05-20.md`、`p4-planner-example-review-2026-05-20.md`；最终均无阻塞/重要问题。
- [x] Prompt P1-P4 验证 — `go test ./... -count=1`、`python -m pytest scripts/test_rag_w0_scripts.py -q`、`git diff --check`、`go build -o "$env:TEMP\compshare-agent-prompt-check.exe" ./cmd` 均通过。
- [x] post-#113 eval Phase C — v2 corrected：qwen3_full 相比 hybrid_cosine 净正向但未达默认切换门槛，#50 defer，#53 提 P1。

## 架构决策
| 决策 | 选择 | 原因 | 日期 |
|---|---|---|---|
| 诊断命令边界 | 允许用户自行执行只读自查命令；修改环境的命令必须标为可选修复 | 既能给用户实际排查手段，又不让助手越权声称执行或默认改环境 | 2026-05-20 |
| SSH 诊断事实源 | 使用 `DescribeCompShareInstance.SshLoginCommand`，不使用 `DescribeCompShareSoftwarePort` 判断 SSH | 上游后端 `SupportSoftwarePort` 当前是镜像应用端口列表，不包含 SSH；SSH 登录入口由实例详情返回 | 2026-05-20 |
| 监控缺失处理 | 缺 CPU/内存/GPU 监控时输出“无法确认”，不按 0% 当正常 | 避免把无数据误判为资源正常或 GPU 空闲 | 2026-05-20 |
| 平台知识来源 | ReAct 主提示不再内置静态 FAQ，平台知识统一走知识库/RAG | 避免 FAQ 字面拷贝与 corpus 漂移，新增知识只维护一份 | 2026-05-20 |
| RAG 提示模板 | 用 `rag_system_segments/order.txt` 组织共享片段，Go/Python 共用 | 避免 runtime 与 eval 双份 prompt 漂移 | 2026-05-20 |
| capability prompt 来源 | `planner_directives` / `planner_examples` 写在 capability markdown frontmatter | 新增能力时 prompt 片段数据化，减少 Go 硬编码 | 2026-05-20 |
| planner examples | 按 intent 分组，逐条记录来源，并测试分组、工具、拦截标记一致 | 降低 25 条例子的维护和审查成本 | 2026-05-20 |

## 已知问题
- [issue] `DescribeCompShareSoftwarePort` 文档示例包含 SSH/FileBrowser=443，但当前后端 `SupportSoftwarePort` 返回的是 ComfyUI/JupyterLab/SD-WebUI/FileBrowser=8889；文档和后端存在差异。
- [issue] qwen3_full 默认切换仍等待 #53 修复后重评。
- [issue] `DeepSeek 400 / reasoning_content` chunk 仍缺失，是 #112 reviewer nit 的独立后续。
