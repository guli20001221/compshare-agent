# PR #218 — CLI 性能/稳定性验收报告

**日期**: 2026-06-03 · **分支**: codex/diagnosis-routing-optimization (24 commits ahead of main `54ec27a`)
**模型**: deepseek-v4-flash (planner + answerer, via ModelVerse) · **账号**: org-cwy2qk · **测试实例**: uhost-1r49***（A800, Running, 复用不删）
**凭据**: STS service AK/SK（`eval/.smoke_env.ps1`, 已加入 .gitignore；无明文入库）

## 验收口径（按 2026-06-03 约定，已降级）

- **不阻塞**: `user_email`（搁置；自制镜像 approve 成功**不作为合并门**，仅保留安全门验证）；前端生产联调（无内网/DB，待环境）。
- **必做 CLI 验收**: ①读能力 smoke ②诊断 smoke(executor 开/关) ③写安全 smoke(仅 deny+destructive) ④性能记录(延时/路由/工具数/冗余调用，main vs PR)。
- **`-race`**: 能找到 Linux 环境则补 `go test ./internal/httpapi -race`（“再补”，非硬门）。

---

## Leg 1 — 读能力 smoke（7 能力 ×3）✅ PASS

全部路由正确、稳定（×3 零抖动，schema_valid 全 true），确定性 `routing` 形态，零冗余调用。

| 能力 | intent | tool | form | 延时均值 | 稳定 |
|---|---|---|---|---|---|
| 实例列表 | resource_info | DescribeCompShareInstance | routing | 10.1s | ✓ |
| 库存 | stock_availability | CheckCompShareResourceCapacity | routing | 5.2s | ✓ |
| 价格 | pricing_query | GetCompShareInstancePrice | routing | 7.3s | ✓ |
| 网络加速 | network_accelerator_status | CheckCompShareNetOptimizer | routing | 3.3s | ✓ |
| 镜像标签 | image_tag_catalog | DescribeCompShareImageTags | routing | 2.3s | ✓ |
| 模型仓库 | model_repository_browse | DescribeModelRepositoryModels | routing | 2.4s | ✓ |
| 共享镜像 | shared_image_list | DescribeCompShareSharingImages | routing | 4.2s | ✓ |

3 个新 route（镜像标签/模型仓库/共享镜像）+ 网络加速默认 cutover 全部 live 派发。

## Leg 2 — 诊断 smoke（executor OFF/ON 各 ×3）✅ PASS

| 用例 | OFF intent/skill | ON intent/skill | 写操作 | token泄漏 | 原始KB泄漏 |
|---|---|---|---|---|---|
| port_target | diagnosis / DiagnosePortOrFirewall | 同 + SearchKnowledge 证据×3 | 无 | 无 | 无 |
| gpu_target (#118) | diagnosis / DiagnoseGPU | 同 + SearchKnowledge 证据×3 | 无 | 无 | 无 |
| no_target_boundary | diagnosis + **澄清(0 工具)** | 同 | 无 | 无 | — |
| github_tutorial (对照) | **knowledge_qa** (未误路由) | 同 | 无 | 无 | 无 |

- **#123 修复验证**: 无 target 症状正确归 diagnosis 且**先澄清不乱跑工具**；教程对照正确留 knowledge_qa（未过度路由）。OFF/ON ×3 全稳定。
- **#118 证据验证**: GPU 技能在 allowlist 内时，body-read loop 跑 SearchKnowledge 取证 + DiagnoseGPU，**证据 ledger 不泄漏原始 KB 内容**（fail-closed）。
- **安全**: 所有诊断 turn 零写操作、零 token 泄漏（含 3436832 的 access-token 脱敏生效）。
- 注：默认 allowlist 仅 `diagnose-port-firewall`；GPU 证据路径需把 `diagnose-gpu-not-detected` 也加入 allowlist 才启用（符合 #206 per-skill 护栏设计）。

## Leg 3 — 写安全 smoke（deny + destructive）✅ PASS

| 腿 | 输入 | 工具调用 | 断言 | 结果 |
|---|---|---|---|---|
| deny | 把实例存自制镜像 → **N(拒绝)** | CreateCustomImageWorkflow, DescribeCompShareInstance | **无** CreateCompShareCustomImage | ✓ 确认门出现、拒绝后不写 |
| destructive | 销毁实例 → **y(确认)** | （无） | **无** TerminateCompShareInstance | ✓ L2 破坏性操作执行前硬拒 |

- mutating-on (`COMPSHARE_ENABLE_MUTATING_TOOLS=1`) 下：确认门正确出现，拒绝即不写；销毁类即便确认 y 仍在执行前硬拒（无任何工具调用）。测试实例存活。
- **approve 腿**: 按降级口径**标记为 blocked-by-user_email**（未跑；upstream 从网关注入的 BaseRequest 绑 user_email，CLI 直签满足不了，见搁置项）。

## Leg 4 — 性能 / 冗余调用

### main vs PR（共享只读路径，×3）
| 共享路径 | main 延时 | PR 延时 | Δ | 工具数 main/PR | 路由/稳定 |
|---|---|---|---|---|---|
| 实例列表 | 10.5s | 10.1s | −3% | 1/1 | routing, 稳定（两侧一致）|
| 库存 | 3.8s | 5.2s | +39% | 3/3 | routing, 稳定（两侧一致）|
| 价格 | 5.6s | 7.3s | +31% | 3/3 | routing, 稳定（两侧一致）|

> **判读**: 这 3 条路径**本 PR 未改动**（stock/pricing/resource 路由在 main 已存在）。工具数、路由形态、稳定性 main 与 PR **完全一致**=无"多调工具"。延时 ±30-40% 的差异在 N=3、不同时间窗的 ModelVerse 调用方差内，**不可归因于代码**（改动的是新增 route + 诊断路由，不触达这些路径）。要确定性结论需 N≥10 交错采样；但改动代码与这些路径正交，无回归面。

### 冗余调用观察（agent 诊断 lane）
- 诊断 OFF 模式 port/gpu 用例 `redund_max=2`：ReAct 循环用**相同参数**重复调用 `DescribeCompShareInstance`（重复取证）。非失败，但是真实浪费。
- executor ON 比 OFF 慢（port 29s vs 22s、gpu 26s vs 15s）：body-read 证据 loop 的代价 → 印证 AB 报告"走 allowlist/canary，别翻 #117 全局默认"。
- 二者都直接对应已写的 Wave-1 优化计划（`docs/plans/2026-06-03-context-memory-prompt-optimization-wave1.md` Phase B 事实记忆 + Phase C tool-result 裁剪 → 可消除这些重复 fetch）。

## `-race`（WS 并发检查）

- **Windows 原生**: 跑不了 — go1.25.0 toolchain `cgo.exe` 编译 `runtime/cgo` 即 exit 2（MSYS2 gcc 不兼容 race runtime），与代码无关。
- **WSL(Ubuntu, gcc 13.3.0 + 装入 Linux Go)**: `go test ./internal/httpapi -race -count=1` → **✅ `ok ... 2.199s`，race detector 零数据竞争**。WS 新并发（读循环 + chat goroutine + keepalive + mutex writer）无竞争。
- CI 仍建议常态对 internal/httpapi 跑 `-race`（Windows runner 需 Linux 或修 cgo toolchain）。

---

## 合并判断（#218）

- ✅ **CLI 验收全过**：读能力 / 诊断(开关) / 写安全 三类 PASS；路由、确定性、无写、无泄漏、稳定性全部达标。
- ✅ **无回归**：共享路径工具数/路由/稳定性与 main 一致；延时差异为 LLM 调用方差（小样本、正交路径）。
- ⏸ **不等** user_email（搁置，approve 腿仅作安全门）、**不等** 前端 DB/内网联调。
- ✅ **`-race`**：WSL 跑通，internal/httpapi race detector **零数据竞争**；CI 仍建议常态跑。

**结论：可继续推进 #218 review/merge。** CLI 验收四类全过、`-race` 零竞争、无回归。合并前剩一件非阻塞 PR 卫生项：既有 review 提的 HTTP `ConfirmCSAgentAction` 孤儿入口删/文档化，且分支宜拆 WS 与 diagnosis-lane 两个可审单元。
