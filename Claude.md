# Claude Code 项目配置：superpowers + gstack + 上下文管理

## 上游 API 参考（查接口/字段/行为时的权威来源）

本项目是 CompShare 平台 API 的下游 Agent。遇到"这个接口要传什么参数、字段怎么解析、
为什么报这个错"时，按下列顺序查上游仓库（本地路径 `F:\uhost-compshare-api-master\`）：

| 目的 | 路径 |
|------|------|
| 接口契约（必填/可选参数、示例、返回格式） | `docs/api/<domain>/<Action>.md`（domain = instance/scheduler/pricing/image/disk/team/spec/model/utils） |
| 接口总览/审查状态 | `docs/api/API接口清单_审查稿.md`、`docs/api/fix_list.md` |
| 真实后端实现（参数校验、错误码、业务流） | `internal/api/compshare/<action>.go`（文件名 snake_case），公共逻辑见 `base.go` |
| 成功调用示例（含 key/project/zone 真实值） | `examples/phaseN/main.go` |
| Block A 之前的调研与技术方案 | `docs/agent/research/`、`docs/agent/plan/AGENT_CONTEXT.md`、`docs/agent/plan/优云AI助手技术方案*.md` |
| Block A 之前的会话日志 | `docs/agent/log/*.md` |

原则：
- 接口行为以 `internal/api/compshare/<action>.go` 的 `preDoWork` 校验为准（文档可能滞后）。
- 需要账户级常量（如 `ProjectId`、`Region`、`Zone`）时先查 `examples/phase*/main.go` 的常量块。
- 不确定字段名时搜 `grep -r "FieldName" F:/uhost-compshare-api-master/internal/api/compshare/`。

## 插件分工

- superpowers：思考与流程（plan / brainstorm / debug / TDD / review / verify）
- gstack：执行与外部世界（browser / QA / ship / deploy / canary / 护栏）

## 任务路由

只走 superpowers：plan / brainstorm / writing-plans / executing-plans / TDD /
systematic-debugging / verification / code-review / subagent / worktrees / 分支收尾

只走 gstack：浏览器 / QA / ship / deploy / canary / retro / document-release /
plan-*-review / investigate / design-* / cso / 护栏（careful / freeze / guard）

浏览器只走 /browse，禁用 mcp__claude-in-chrome__* 和 mcp__computer-use__*。

## 任务分级
- 只读/轻量（单文件、明确 bug）：直接做 + 定向验证
- 中（多文件、边界清晰）：简短 brainstorm + 短 plan + 实现 + /browse + verification
- 大（跨模块、新架构）：完整闭环 brainstorm → plan → review → TDD → /qa → verify → code-review → /ship

## 五条铁律
1. 浏览器只走 /browse
2. verification 和 code-review 必须分两个独立上下文
3. 没有测试/截图/QA 报告不算完成，禁止虚构命令输出
4. 创造性任务先 brainstorm（改 typo / 明确 bug 除外）
5. 危险命令（rm -rf / DROP / force-push / reset --hard）先 /careful

## 子 agent 策略
派：2+ 独立无共享状态的任务 / 纯研究搜索 / 单文件分析 / 独立验证 / git 历史分析
不派：有顺序依赖 / 改同一文件 / 需要用户交互 / <3000 tokens 的轻量操作

## 上下文管理

### 子 agent 汇报
完成后返回结构化摘要（500-800 tokens），详细产物写入 .claude/artifacts/<任务名>.md。
摘要格式：状态 → 结论（1-3 句）→ 变更文件 → 关键决策 → 发现 → 遗留 → 产物路径

### CONTEXT.md 维护
每个项目维护 `.claude/CONTEXT.md`，新对话开始前先读取。

写入时机：中/大任务完成后 / 多轮工具调用后 / 用户要求 checkpoint / 对话结束前
文件结构：

```markdown
---
last_updated: 2026-04-13T15:30+08:00
---
# Project Context
## 当前状态
[一段话概括进展]
## 进行中
- [ ] 任务名 — 阶段 / 已完成 / 下一步 / 阻塞
## 最近完成
- [x] 任务名 — 关键决策 / 变更文件
## 架构决策
| 决策 | 选择 | 原因 | 日期 |
## 已知问题
- [issue] 描述 — 优先级
```

更新规则：覆盖写入不追加 / 控制在 2000 tokens 内 / 超过 10 条的已完成条目删除 /
artifacts/ 下超 7 天的文件 checkpoint 时清理

### 压缩保护
压缩时始终保留：修改文件列表、测试命令、架构决策、未解决的 bug。
不同任务间用 /clear 隔离。