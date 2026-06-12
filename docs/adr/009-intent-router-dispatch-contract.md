# ADR-009: Intent-Router 与 Dispatch-Contract 边界

**Status**: Proposed (2026-06-09)
**Depends on**: ADR-003(skill ⊥ tool)/ ADR-007(不引 framework,自写 orchestrator)
**Amends**: ADR-001(task-tier architecture)——supersede 其 **planner-emits-`task_tier` / planner-emits-lane** 部分(ADR-001 Decision「Tier 选择是 planner 输出的 first-class 字段」+ Acceptance「Planner 输出 `task_tier` 字段」)。**ADR-001 的三个 runtime form(fast / knowledge / agent)仍保留不变**;被修订的只是"由 LLM 输出 tier / lane"这一实现方式——tier / runtime 归属改由 `DispatchSpec` + `ResolveDispatch` + trace **投影**,LLM 只吐 `intent + slots`。
**Plan**: `docs/plans/2026-06-09-intent-router-dispatch-restructure.md`(完整 6-PR 序 + 各组分瘦身 + 行号锚点)

## Context

当前被称作 **`Planner`** 的组件,按结构是一个 **intent Router**:`Plan`(`internal/intent/types.go:109-123`)是 flat classification object,`Planner.Plan()` 一次 LLM call 产出一个 intent label;**无** subtask / risk / approval / success_criteria / replan。多步推理在其后的 ReAct loop 与固定 saga(`deploy_model.go` `CreateInstanceDef()` Go 字面量 `[]Step`)里。

问题不是"缺 planner",而是 **routing 真理散落三处**:① 命名(真 intent-router 无包,埋 `engine.go:1433-1535`;"router" 名被 PreBlock / `llm/router.go` 占);② planner prompt(`buildSystemPrompt` 26 边界规则中 ~16 条是纯 lane-routing → 分类器 prompt 变 NL 路由表,每改一条 = SHA bump + jitter 回归);③ dispatch if-chain(+ `isUnsupportedHistoricalMonitorQuestion` 关键词覆盖 = 第二真理源)。因错名,PR #97/#128 持续往分类器塞 routing。

同时:Anthropic / OpenAI 的 Agent Skills **渐进披露**(L0 metadata → L1 `SKILL.md` → L2 resources)在本仓已实现(`internal/skills/loader.go`),但本产品有真实租户身份、写操作、计费 / STS 边界、固定 saga,**不能照搬 free skill discovery**(把所有 skill metadata 常驻 + 模型自由选择 + skill 影响工具授权)。

本 ADR 钉住三条边界,作为常驻 reference,防未来 PR 把分类器 / skill 重新变成 routing 真理源或工具授权源。

**与 ADR-001 的关系**:ADR-001 的三个 runtime form(fast / knowledge / agent)**保留不变**;本 ADR 仅**修订**其"由 planner 输出 `task_tier` / lane"的实现方式(详见 header `Amends`)——tier / runtime 归属改为 `DispatchSpec` + `ResolveDispatch` + trace 的确定性投影,不再是 LLM 输出字段。这避免 ADR-001 与 ADR-009 在"planner emits tier/lane"上留下明面相反结论。

## Decision

### D1. IntentRouter 只做语义路由,不做 plan / 执行 / 工具授权

`Planner → IntentRouter`,`Plan → IntentRoute`(非 `RouteDecision`——Decision 暗示含 execution)。产物只有 `{Intent, Slots}`(confidence / reasoning 为 observe-only)。它 **不** emit lane / execution_path / handler / tool authorization。**不新增 AgentPlanner**,**不做 planner-emits-lane**(#128 PARK,与"LLM 只吐 intent+slots"目标冲突)。系统保持 ADR-001 的"确定性 workflow 层 + single-agent ReAct 层"——对稳定 GPU 操作,**没有 AgentPlanner 是正确默认,不是缺陷**。

### D2. DispatchSpec 是名义(nominal)纯契约;effective runtime form 单独 resolve

`DispatchSpec = intent → {NominalLane, ToolSubset, AgentSkillName}` 的**纯投影**,只委托现有三面(`PlannedExecutionPathForIntent` / `IntentToolSubset` / `agentSkillForIntent`)。它**不读 runtime 状态、不闭包、不执行**。运行时差异——flag gate、snapshot count、screenshot suppression、per-engine enable——由 `ResolveDispatch(route, runtimeCtx) → DispatchDecision{EffectiveExecutionPath, RouteStatus}` 解析。

- **红线**:`PlannedExecutionPathForIntent(knowledge_qa) = terminal_rag` 保持纯;agent-loop override 只活在 trace / resolve,**绝不进 spec**。
- **红线**:`DispatchSpec` 条目禁 capture-runtime-state 闭包(`HandlerKey string` / `HandlerKind enum` 可;knowledge_qa 三标志 AND-gate 闭包不可 = 第二 planner)。
- **不是"一张纯表吃掉所有 routing"**:只有 bucket A(per-intent 纯事实)入 `DispatchSpec`;bucket B(跨 intent tie-breaker)入 `BoundaryPack`/`TieBreakerPack`(prompt 投影);bucket C/D(分类核心 / 输出契约)永留 Router base prompt。

### D3. Skill 不授权工具;披露是 contract-gated 渐进披露

工具授权归 `VisibleRegistryForSubset` + `SafeToolExecutor`(L2)+ policy;skill body 只 *request*。不变量:`loadedSkill.RequiredTools ⊆ DispatchSpec.ToolSubset`(测试**补齐**,主线只有 route 层 parity、无 body-read skill 通用测试)。

渐进披露发生在 **Dispatch 之后、Executor 之前**,且在 `DispatchSpec` 限定的候选 / 工具边界内 —— **`DispatchSpec` 是 Agent Skills 的上游控制面,不是替代**。`BoundaryPack`(router-time 分类边界)≠ `Skill`(execution-time 方法论),分目录(`internal/boundarypacks/` vs `internal/skills/`)。"模型从候选 skill metadata 中选择"(`CandidateSkills` / `SelectionMode`)延后到 **v-next**:今天只有 `deploy_model` pinned-by-intent,diagnosis 走确定性 `diagnosisSkillExecutorPilotForAction` + allowlist,无候选 selector;**无 eval 不建**。

## 反向论证:什么会改变这些决策

- **D1**:步骤集随请求真可变 + 组合爆炸 + eval 证固定 saga 漏步(首候选:多资源集群开通)→ 引入 AgentPlanner,amend D1。
- **D2**:某 intent 的 nominal 真依赖 runtime(而非可投影常量)→ 该项移出 `DispatchSpec` 进 `ResolveDispatch`,**而非**给 spec 加闭包。
- **D3**:出现真正"模型从 N 个候选 skill 选择"需求且有 eval 背书 → 启用 `CandidateSkills` / `SelectionMode`,扩展 D3(仍保 ⊆ 授权不变量)。

## Consequences

**Positive**
- 单一 dispatch 契约可读、parity-tested;prompt 退化为契约渲染,if-chain 退化为 runtime guard;每轮 context 变小。
- 命名不再诱导往分类器塞 routing。
- skill 边界清晰:Anthropic-style 渐进披露与企业级控制面并存,授权集中在一处。

**Negative**
- 6-PR 序列跨多个边界,需逐 PR byte-stable 验证。
- BoundaryPack 试点起有意 SHA bump(per-pack SHA + jitter 锚点手动 PS 维护,非 CI)。

**Risks**
- 未来 PR 误把 knowledge_qa AND-gate 闭包塞进 `DispatchSpec`(= 第二 planner)→ 红线测试 + review 枚举防。
- rename 经 registry-derived prompt fragment 渗入 SHA(`registry-derived-prompt-fragment-rename-bleed`)→ PR4 枚举全部 SHA delta 源、冻结 `planner.go:577` 那句 prompt role。

## Acceptance

- [ ] PR1 落 `internal/engine/dispatch_spec.go` 纯投影 + parity(`intent.AllIntents()` deep-equal),`systemPromptSHA256Baseline` 不变(post-#251 main = `64dc6a4c…`,PR 时复读)。
- [ ] PR2 补 body-read skill `RequiredTools ⊆ ToolSubset` 测试,绑实际 pilot / allowlist 路径(非假设 `CandidateSkills`)。
- [ ] PR3 ExecutionContract 显式 `static | dynamic | none` tool binding,dynamic `ToolFunc` / `StepConfirm` 步 `RiskKnown=false`(不伪造 risk)。
- [ ] #128 在 task tracker 标 parked。
- [ ] 本 ADR 加入 ADR index(若仓库建立 `docs/adr/README.md` 时补齐;当前仓库无 ADR index);新人入职 review。

## References

- Plan: `docs/plans/2026-06-09-intent-router-dispatch-restructure.md`
- ADR-001(task-tier)/ ADR-003(skill ⊥ tool)/ ADR-007(framework anti-pattern)
- Anthropic, "Equipping agents for the real world with Agent Skills";OpenAI, "Skills"(progressive disclosure;skill instruction 是 user-prompt input,**非** system-level policy → 授权必须在 dispatch / registry / executor 层完成)
- 全局 CLAUDE.md Rule 2(Simplicity First)/ Rule 7(Surface conflicts)/ Rule 11(conformance > taste)
