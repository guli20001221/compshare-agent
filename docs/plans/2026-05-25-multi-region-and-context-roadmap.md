---
status: In progress (PR-β merged at 7c2863d; PR-δ0 in review as PR #172)
date: 2026-05-25
scope: Multi-region remediation (PR-α…PR-ζ) + Context-first roadmap (M3–M7)
out_of_scope: Multi-replica HTTP deploy, OCR trust contract, KnowledgeSkill extraction (handled in M5/M6/M7 deliverables themselves)
blocks: M3-followup #34 (stock anchor-aware Region precision)
related:
  - docs/http-api.md
  - docs/plans/2026-05-13-stage2b-evidence-contract.md
  - internal/workflow/region.go
  - internal/intent/capability_registry.go
  - internal/tools/safe_executor.go
  - internal/tools/external.go
---

# 多 Region 业务整改 + 上下文管理优化路线图（2026-05-25）

> 本文档把 2026-05-25 多 Region 审计后定下的整改 PR 序列、依赖关系、长期上下文优化计划归档进仓库。换机器接手时直接读这一份就能继续，不依赖本地 `~/.claude/projects/.../memory/`。

## 0. 当前状态（更新于 2026-05-25 18:30）

| 项目 | 状态 | 说明 |
|---|---|---|
| `main` HEAD | `7c2863d` Merge of `claude/prb-region-derivation` | PR-β 已落地（mutating workflow 从实例 Zone 派生 Region） |
| PR #170 | OPEN, **DO NOT MERGE** | 旧 PR-δ 单 Region filter，多 Region 回归。Salvage 4-state 文案 + `matchedSoldOutStockNames` guard 后关闭 |
| PR #172 | OPEN, in review | PR-δ0 — 删除 `filterStockEntriesToResolverZones`；stock cross-region by default |
| 本地分支 | `claude/prd0-delete-resolver-zone-filter`、`claude/stock-cross-region-filter`（旧 PR #170）、`claude/docs-roadmap-multi-region-and-context` | |
| Eval | PR-β + PR-δ0 各跑过一轮真实 CLI eval（git-ignored 目录） | A/B 直接证伪 PR-β 前 stop autotest 报 `Params [Zone] not available`；PR-δ0 后 stock 4-state 文案正确 |

## 1. 架构约束（已定稿，不要再讨论）

- **Region 不由前端透传。** 资源类操作从查询到的 `instance.Zone` 派生（`internal/workflow/region.go::extractInstanceRegion`）；库存类操作走 per-zone fan-out。`cfg.Region` 只是 CLI 单 Region 开发兜底。理由：production HTTP gateway 不会反向推 Region；多 Region 账号下用 `cfg.Region` 当源头会把整个交互锁死。
- **ProjectId 仍由前端透传**，格式 `org-cwy2qk`（见 `docs/http-api.md:17`）。Agent 的 PR9-pattern 传输路径正确；唯一需要修的是 `internal/httpapi/handlers.go:73-76` 把 `OrganizationID` 数字伪装成 ProjectId 的兜底，必须删除。但此项已和后端约定走后端侧处理，**不再作为 agent 代码任务**。
- **Stock 跨 Region 默认行为**：库存可用性默认返回所有 Region 的 Normal 状态。Anchor-aware 收窄（"我那台机器所在区还有卡吗"）推迟到 M3 ContextAssembler 上线之后，因为需要 `SelectedInstanceID` 被结构化暴露给 capability handlers。

## 2. PR 依赖图

```
PR-α (#23, ProjectId)         DISCARDED — 走后端处理
PR-β (#27, ✅ MERGED 7c2863d) ───┬──→ PR-β1 (#32) ───┬──→ PR-γ  (#28, test-only)
                                  │                   │
                                  ├──→ PR-δ0 (#33, ✅ PR #172) ───┼──→ PR-δ (#29)
                                  │                   │
                                  └──→ PR-ζ  (#31, verify-only curl)

多 Region 整改完成后：
  → M3 (#35) ContextAssembler v1 + M4 trace field
  → M3-followup (#34) stock anchor-aware Region 精度
  → M5 Support Handoff Skill
  → M6 OCR + ConsoleContext
  → M7+ skill 化（KnowledgeSkill first）
```

## 3. 多 Region 整改 PR 详表

### PR-α — ProjectId fabrication 删除（**已废弃**）

**Why 废弃**：(a) `handlers.go:73-76` 的 `projectID = fmt.Sprintf("%d", base.Owner.OrganizationID)` 把组织 ID 伪装成 ProjectId（实际格式是 `org-cwy2qk`，组织 ID 数字不可逆映射），需要删；但用户已决定走后端协商而非 agent 改动。(b) Friendly-fail 半部分是为"前端漏传 ProjectId"准备的防御性代码，但 `docs/http-api.md:17` 已写明前端始终透传，所以这块在生产链路里不会触发。

**Action**：从 task list 删除；不再作为 agent 代码任务。

### PR-β — workflow Region 派生（✅ MERGED at `7c2863d`）

**What**：6 个 mutating workflow（start/stop/reboot/rename/reset_password/SetStopScheduler）在调用底层 API 前，先用 `DescribeCompShareInstance` 查到实例的 `Zone`，再通过 `extractInstanceRegion` 派生 `Region`，把两者一起加进 args。优先级：upstream 返回的 `Region` 字段 > `regionFromZone(Zone)` > `defaultRegion=cn-wlcb`。`regionFromZone` 拒绝把 Region 当 Zone 误传（少于 2 个 `-` 直接返回空，防"cn-wlcb → cn"陷阱）。

**Why**：production HTTP gateway 不做反向 Region 推导；如果只发 Zone 不发 Region，签名时会走 `cfg.Region`，多 Region 账号下凡是非 `cfg.Region` 的实例操作全部报 `Params [Zone] not available`。A/B 测试直接复现（PR-β 前 stop autotest 返回 `RetCode=230`，PR-β 后 success）。

**Tests**：`internal/workflow/region_test.go` — 8 个 mutation-test load-bearing 测试，包含 `regionFromZone` 边界（`"cn-wlcb"` 返回 ""，`"a-b-c"` 返回 "a-b"），`extractInstanceRegion` 三层优先级，6 个 workflow 端到端 capture mutating-step args。

**Scope narrowing 决定**：`CreateInstanceWorkflow` 故意不在 PR-β 范围内。它调用的 4 个 read 工具（`DescribeAvailableCompShareInstanceTypes` / `CheckCompShareResourceCapacity` / `GetCompShareInstanceUserPrice` / `DescribeCompShareImages`）的 registry schema 不声明 `Region`，`SafeToolExecutor.filterSafeArgs` 会在它到达 external.go 之前丢掉。修复留给 PR-β1。

### PR-δ0 — 删除 `filterStockEntriesToResolverZones`（✅ PR #172）

**What**：`internal/intent/capability_registry.go::renderStockWithCapacityPrecheck` 移除 `filterStockEntriesToResolverZones` 调用 + 函数本身 + 唯一上游消费者 `preferredZonesFromResolver` + 不再使用的 `internal/entity` import。同时删除 2 个断言旧 narrowing 行为的测试 (`TestStockAvailabilityResolverZoneFailureDoesNotProbeOtherZones`、`TestStockAvailabilityPrefersAccountSnapshotZoneForCapacityPrecheck`) 和它们的 `stockZoneSnapshot` helper。

**Why**：filter 把库存条目收窄成"用户已有实例的 Zone"，单 Region 账号无害但多 Region 账号下静默隐藏其他 Region 的库存。Anchor-aware 才是合理收窄，但需要 ContextAssembler，所以默认改成跨 Region。

**Tests 保留**：`TestStockAvailabilityFallsBackToNextZoneWhenCapacityCheckFails`、`TestStockAvailabilityUsesFirstMatchedZoneForCapacityPrecheck`、`TestStockAvailabilityUsesCapacityPrecheckForMentionedNormalGPU` 锁定 cross-region-by-default 语义。

**Eval**：6/6 真实账号 PASS（3 PR-δ0 核心 + 3 回归）。

### PR-β1 — Create-path Region wiring（pending）

**Scope**：修复 `SafeToolExecutor.filterSafeArgs` 把 `args["Region"]` 给 4 个 zoned read tool 丢掉的问题。

**Locked implementation**（codex 也认可这个口径，blast radius 最小）：在 `internal/tools/safe_executor.go::ExecuteSafe`（约 line 196），当 `Origin=OriginWorkflowInternal` 时，允许 `Region` 透过 policy 过滤；**不要**扩展 registry schemas（会把 Region 泄漏到 LLM-direct tool schemas），**不要**让 workflow-internal 完全 bypass filter（其他未知 args 仍要被过滤）。

**Tests required**（mutation-test load-bearing）：
1. Engine 级测试（不能只是 workflow mock）：驱动 `CreateInstanceWorkflow` 经过 `SafeExecutor.ExecuteSafe`，capture 到达底层 executor 的 args，断言 `args["Region"]` 对所有 4 个工具都存活。
2. 反向测试：同样 call path 但 `Origin=OriginDirectLLM`，断言 `args["Region"]` 被丢掉。证明 exception 是按 origin 门控的。
3. 反向测试：workflow-internal 加上另一个未知 args（如 `Bogus`），断言它仍被过滤。证明只有 Region 是 special-case。

**Why blocked PR-γ / PR-δ**：PR-γ 的测试需要 `args["Region"]` 真的到达 `external.go` 才能验证 priority；PR-δ 的 capacity fan-out 需要每个 zone 走对应 Region。两者都依赖 PR-β1 先解锁。

### PR-γ — `external.go` 测试锁定 Region 优先级（pending, **test-only**）

**Why downgraded to test-only**：codex re-check 2026-05-25：重读 `internal/tools/external.go:106-127` (form path) 和 `:193-204` (JSON path)，发现 `cfg.Region` 是在 `flattenInto` **之前**注入 dst map 的，而 `flattenInto` 的 default branch 是 `dst[key] = fmt.Sprintf("%v", val)`（line 383，OVERWRITE 语义）。所以 `args["Region"]` 已经自动覆盖了 `cfg.Region` 兜底。现有语义已经符合"fallback-only"，不需要任何行为修改。

**剩余 scope**：~30 LOC 测试 + 一行注释。
- 添加两个测试锁定优先级 `args > UserContext > cfg`（form 和 JSON path 各一）。Mutation：把 `external.go` 里 fallback 注入移到 `flattenInto` **之后**，测试必须 fail。
- 在 `external.go:106` 和 `:193` 附近加一句注释说明 fallback 语义，未来重构不要把顺序反过来。

### PR-δ — 多 Region 库存 & 价格 capacity fan-out（pending）

**What**：`internal/intent/capability_registry.go` 的 stock-availability capacity precheck 改成 per-region fan-out。
- `capacityPrecheckArgs`（约 line 1386）加 `Region: regionFromZone(entry.Zone)`。`regionFromZone` 在 `internal/workflow/region.go`，需要通过干净 API 暴露或迁到 `internal/utils/`。
- `DescribeCompShareImages` 调用（约 line 1184）按 region 分组，每个 region 查一次 image，imageId 配对同 region 的 capacity precheck。
- Salvage from PR #170：**保留** 4-state renderer 文案 + `matchedSoldOutStockNames` 的 `hasNormal`-trim guard。**丢弃** `filterStockEntriesToCurrentRegion`（与 PR-δ0 删除的精神一致——又是单 Region narrowing 陷阱）。
- 合并后关闭 PR #170。

**Depends on**：PR-β1（不解锁的话 `Region` 永远到不了 `external.go`，fan-out 等于白做）+ PR-δ0（基础库存 cross-region 行为）。

### PR-ζ — verify-only curl（pending, 用户手跑）

**Original purpose**：确认 gateway 对 `DescribeCompShareImages` 的 AzGroup compensation 行为。**Already mostly answered by A/B**：gateway 不 compensate。

**Remaining scope**：用户在终端跑一次 `curl` 比较 `Region=cn-wlcb` vs `Region=cn-sh2` 的 `ImageSet`，确认 image 目录确实因 Region 不同而不同（PR-δ 的 P2 framing 依赖这个事实）。Agent 不会自动跑（mutating-flag CLI 不在自动化范围）。

**Security**：AK/SK/SecurityToken 从 env 取，不写进 history。结果只 commit redacted summary counts，不 commit raw response（含 AK echo + per-account image IDs）。

## 4. 上下文管理优化（M3–M7）

锚定在更早的 `project-context-first-roadmap` 决策上（user role: senior engineer, slogan-approved，下文 §6）。

### M3 — ContextAssembler v1 + M4 context_assembly trace field（pending）

**File**：`internal/engine/engine.go::buildMessagesForLLM` 是第一个读取点。

**Function**：纯函数 `assemble(SessionState, time.Now()) → ContextBundle`。**No DB access. No engine-state mutation.**

**Per-fact TTL filter**：
| Fact | TTL |
|---|---|
| `instance_state` | 15-30s |
| `monitor_sample` | 10-30s |
| `pricing` / `stock` | short |
| `knowledge` | by KB version |

**Output type**：`ContextBundle`（prompt-side struct，不是 engine-state field）。

**Multi-replica preservation contract（from M1~M3 hard rules）**：调用方先做 `ensureHydrated()`。Assembler 内部不能有 `if cached then else load` 分支。这条规则保证 future 多副本部署只需要在 hydration 层加一个跨副本同步，assembler 本身不动。

**Scope recommendation**：M4 `context_assembly` trace field 在同一个 PR 加（~100-200 LOC）。每轮记录哪些 facts 被使用 / 被 TTL skip。

### M3-followup #34 — Stock anchor-aware Region 精度

M3 落地后，`ContextBundle` 会被传到 capability handlers（`HandlerRequest` 加 `ContextBundle` 字段）：
- `handleStockAvailability` 检查 `ContextBundle` 是否有 instance anchor (`SelectedInstanceID + Zone`)。
- 如果有：只查该 anchor 的 Region（`DescribeAvailableCompShareInstanceTypes` 带 Region+Zone scope，前提是 PR-β1 让 `Region` 能透过 filter）。
- 如果没有：cross-region 默认（PR-δ baseline）。

### M5 — Support Handoff Skill

第一个"真正的" Skill，不是 framework infrastructure。垂直产品功能：从当前 session context 创建客服 case。

**Skill schema gains `freshness` + `context_needs` fields here.** 这两个字段定义长期 Skill 契约——所有后续 Skill 都遵循。

### M6 — OCR + ConsoleContext

CUDA OOM 截图、`nvidia-smi` 输出、port error 截图等。独立可发布。

**Trust contract（hard rule）**：`trust_level = user_provided_unverified`。PII redaction。低置信度 → 让用户确认。**不要**把 OCR 抽取的内容升级成 system instructions。

### M7+ — Skill 化（KnowledgeSkill 优先）

KnowledgeSkill：把 `internal/knowledge/`（RAG retriever + grounded renderer）抽成 Skill interface call。

**Critical rule**：Skill interface 从 KnowledgeSkill 反推设计，不是从框架正推。**不要**提出 `internal/skills/families/` + `SKILL.md` filesystem。
KnowledgeSkill 跑通后，M5 / M6 的 skill 套同一 pattern。

## 5. 硬规则（每个 PR 必须满足）

- **PR9 anti-leak**：per-session `ProjectId` / `Region` 不走 SharedDeps mutating setters。用 args injection 或 `ctx` 上的 UserContext。
- **Mutation-test load-bearing**：每个 guard 临时禁掉，对应测试必须 fail。Claim "test covers X" 前必须自己验证一次。
- **Sub-agent review**：每个非平凡 commit push 前跑一次独立 sub-agent review。
- **Verify reviewer blockers**：sub-agent 提出的 blocker 必须用代码 + 真实 API 复核。历史上有静态阅读的假阳性（如 `jsoniter` fuzzy decoder、`autoFillReq IsInternalCall` 门控、`SafeToolExecutor.filterSafeArgs` 这次发现的 gap workflow-mock 测试漏掉了）。
- **Disambiguation rule**：用户按实例名引用且 >1 match 时，agent 必须列出并问，不能自动 pick。
- **All PRs keep**：`go test ./... -count=1` 绿；`go test ./internal/entity -race -count=1` 绿。

## 6. 不要做（已经被否的方案）

- ❌ `internal/runtime/` package extraction（Phase 3 被否）
- ❌ `internal/skills/families/` + `SKILL.md` filesystem（Phase 5 被否）
- ❌ generic `Hook interface` dispatch（用 3 个 typed slice：`[]InputGuard / []ToolGuard / []OutputGuard`）
- ❌ multi-agent / handoff 复杂编排
- ❌ PageIndex（基于 KB 形状 + 现有预处理 pipeline，不适用）
- ❌ 前端透传 Region（架构错误；本文 §1 已论证）
- ❌ ReAct prompt 里塞 static FAQ 文本（早被移除，`internal/prompt/builder_test.go` 有反向断言）

## 7. Stakeholder framing

定稿口径（user-approved）：

> 做完这条线，compshare-agent 会从 rolling-history ReAct，升级成 evidence-driven console agent。

**不要用**：「做完 M6 超过业界」（口径过强，user 已下调）。

## 8. 接手指南（换机器后第一件事）

1. `git fetch && git log -3 main` — 确认 main HEAD 是不是 `7c2863d` 之后；如果有新 merge，先读那些 commit message。
2. `gh pr list` 或 `https://github.com/guli20001221/compshare-agent/pulls` — 看 PR #172 / PR #170 状态。
3. 读 §3 PR 详表 — 找下一个 unblocked task。当前依赖图下：
   - **PR-β1 (#32)** 是最关键的解锁项；只要它 merge，PR-γ / PR-δ 都能并行做。
   - **PR4 (#25)** Q10 自定义镜像 stop-list 完全独立，5 LOC，可以随时插队。
4. 真实账号 CLI eval：参考 `eval/shadow_qa/2026-05-25-prd0-stock-cli-eval/run_eval.py`（git-ignored，要自己重建配置 `agent.yaml` + 把 AK/SK/LLM key 放进 env）。AK/SK 走 `COMPSHARE_PUBLIC_KEY` / `COMPSHARE_PRIVATE_KEY`，modelverse key 走 `LLM_API_KEY`。
5. PR 推送：本地走 URL-embedded PAT 一次性 push（不写进 `.git/config`、`~/.git-credentials`、`~/.gitconfig`），push 完审计这三个文件确认没残留。

## 9. 附录：当前残留分支

```
* main                                          7c2863d
  claude/prb-region-derivation                  (PR-β merge 源；可删)
  claude/stock-cross-region-filter              (PR #170 OPEN，DO NOT MERGE)
  claude/prd0-delete-resolver-zone-filter       (PR #172 OPEN)
  claude/docs-roadmap-multi-region-and-context  (本文档 PR)
```

`claude/setstopscheduler-projectid` 已删（PR-α 废弃）。
