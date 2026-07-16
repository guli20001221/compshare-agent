# P0 — 收敛计划验收基线（冻结现状）

对应计划：`2026-07-16-agent-runtime-harness-convergence-plan.md` 的 P0。
本文件只记录改造**前**的可复现现状，作为 P1–P6 的回归判据。不新增关键词、不新增固定拒答。

## 基线坐标

- 分支：`codex/agent-runtime-p9a`
- 头提交：`297ad8b1`（`refactor(agent): converge context-aware runtime`）
- base：`origin/main@b5bbcd75`
- worktree：`F:/compshare-agent/.worktrees/agent-runtime-p9a`
- 工具链：go1.25.0 windows/amd64（仓库声明 Go 1.22，向后兼容）

## 全量测试基线（回归主判据）

`go test ./... -count=1`（`COMPSHARE_PROJECT_ID` 置任意非空值以过 `TestLoadConfig`）：

- 结果：**全绿**
- 47 个包 `ok`，8 个包无测试文件
- **0 FAIL / 0 panic / 0 --- SKIP**

这套绿色套件已经包含所有确定性行为基线，改造后必须保持全绿：

| 基线用途 | 载体 |
|---|---|
| Prompt/系统卡形状 | `internal/prompt/react_prompt_snapshot_test.go` |
| 中央 Agent 工具窗口 | `internal/engine/central_agent_runtime_test.go`（pin 工具集，`DescribeCompShareInstance` 不在窗口内） |
| 端到端场景 | `internal/engine/scenario_test.go`、`eval/golden_test.go`、`eval/evaluate_test.go` |
| 执行路径矩阵 | `eval/execution_path/execution_path_matrix_test.go` |
| HTTP 会话回放 | `eval/realism/http_session_replay.go` |
| 上下文回放 | `eval/contextreplay/main.go` |
| trace 门禁 | `eval/trace_gate/*` |

## 架构门禁基线（P6 参考量）

`internal/architectureguard/baseline.json`（`TestNoNewSemanticRegexKeywordOrDecisionSite`）当前实测：

- `regex`：**108**
- `string_heuristic`：**231**

> ⚠️ 计划 P6 正文写的是 `regex:106`，实测为 **108**，计划数字过期 2。P6 以本文件的 **108** 为准。

门禁语义（已核 `scanner.go`/`inventory.go` + `docs/dev/agent-runtime-migration-guard.md`）：

- 基线是"允许存在的最大集合"。`Unexpected(current, reviewed)` 只标记"当前有、基线没有"的点。
- **纯删除永远安全**：删除 regex / `strings.Contains|HasPrefix|HasSuffix` 站点不会产生新 finding，套件自动保持绿，无需改 `baseline.json`。
- **新增受门禁**：新增任一上述调用，或把符号改名成匹配 `semanticSymbolName`（`inferXxxAction`/`resolveContextDecision`/`taskSlot`/`tryDirect`… 等）的名字，会 FAIL，除非确认属于协议/安全/严格实体格式并同步更新基线。
- 扫描范围仅 `internal/**`，跳过 `_test.go`、`registry_gen.go`、`internal/architectureguard/`。

因此 P5/P1b/P6 的删除工作不需要改 `baseline.json`；只有 P1c/P2/P3/P4 若引入新的确定性字符串检查才需要评估。

## 迁移清单基线

`docs/audits/agent-runtime-migration-inventory.json`（`TestMigrationInventoryStillNamesLiveProductionSites`）：

- 仅 1 条：`internal/engine/engine.go::ChatWithOptions`，needle `enginePreBlock.Decide`，category `security`，phase `P6a`。
- 该项是**安全前置阻断**，计划明确保留；本轮所有删除（Saga Runner、在线 Verifier、`deploy_model.go` 孤儿）都不触及它。
- 纪律：任何删除若命中清单符号，必须同步删除/改目标该清单项，不得改名规避。

## 工作区无关改动（不属于本计划）

改造开始时工作区已有 34 个 `docs/plans/*.md` 的**未提交删除**（环境既有状态，与本计划无关）。执行期间只 `git add` 本计划实际改动的文件，绝不 `git add -A`，避免把这些删除混入收敛提交。

## P0 场景验收清单（P7 实测判据）

以下场景是 P7 真实 HTTP/WebSocket 回放的验收对象。P0 阶段它们的确定性形态由上表绿色套件冻结；**真实上游 + 真实 LLM 的端到端回放是 P7 门槛，不在 P0 伪造**（计划第 9 条：单测通过 ≠ 收敛完成）。

- [ ] "我有哪些实例"
- [ ] 实例查询后发生无关 / 空命中 `SearchKnowledge`，实例结果不被知识守卫删除
- [ ] RAG 命中 / 无命中 / 引用错误
- [ ] "添加数据盘"→"200G" 与 "加200G数据盘"
- [ ] 创建实例指定 GPU / 镜像 / 可用区
- [ ] 创建失败后的镜像替代（执行前重新确认）
- [ ] 单轮连续多个较大工具结果，上下文有界
- [ ] 冷启动恢复 / 中断恢复 / 重复提交
- [ ] 账户余额 / 流水 / 发票状态 → 结构化"当前能力不可用"，不虚构数字
