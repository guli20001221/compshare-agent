# Architecture Refactor Roadmap (B1–B8)

**Status of record.** This file is the version-controlled batch roadmap for the
task-tier architecture refactor. It records *what ships in which batch, in what
order, and what blocks what* — the sequencing layer. It does **not** restate the
design: the canonical design lives in `docs/adr/001`–`007`. When a batch lands,
update its status row here.

> History note: the original batch breakdown lived only in a session memory note
> (`architecture-refactor-blueprint-2026-05-28`), not in the repo. That caused a
> real drift (a stale note claimed the `task_tier` gap was HTTP-only; it is both
> paths — see B4 below). This file exists so sequencing decisions are reviewable
> in git, not memory-only.

## Batch status

| Batch | Scope | ADR | Status | Commits / Spec |
|---|---|---|---|---|
| **B1** | Safety pad: dead-code cleanup + `internal/llm/router.go` tier-aware wrapper + reserved trace `task_tier` slot | 002 | ✅ shipped | `29029c4` + `731545f` (rev2) + `db5db03` (rev3) |
| **B2a** | Router inject completion: `engine.NewSharedDeps` + 2 eval sites consume `Router.For(TierFast)` (build-inside, signature unchanged, byte-stable) | 002 #3 | ✅ shipped | spec `a713790`, impl `b38fe28` — `docs/plans/2026-05-29-b2-router-inject-completion.md` |
| **B2b** | Skill/Tool directory split + codegen + **progressive disclosure** (planner prompt → metadata-only) | 003, 004 | ⏳ spec to draft | outline in the B2a spec §B2b |
| **B3** | Fast path drops the LLM grounded renderer (handler → template + envelope constraints) | 001 | ⏳ pending | — |
| **B4a** | Observability: derive **realized tier** from dispatch path; populate a trace field at the shared write point | 001 #4 | ⏳ pending | see "B4 decomposition" |
| **B4b** | Planner emits **predicted tier** (new output field + prompt/schema change); planner model → pro; N≥20 regression | 001 #4, 002 | ⏳ pending (gated on B2b) | see "B4 decomposition" |
| **B5** | Diagnosis package, k8sgpt-style (Analyzer/Failure/Filter) | 005 | ⏳ pending | — |
| **B6** | Agent path infrastructure: orchestrator + saga + multi-step HITL + SSH category sandbox | 006 | ⏳ pending | — |
| **B7** | MCP gateway (in-process + external stdio/HTTP entry) | 003 Amendment 1 | ⏳ pending | — |
| **B8** | First end-to-end agent skill ("deploy Qwen32B" / "SSH debug") | 006 | ⏳ pending | — |

Each batch ships independently with its own CLI smoke + regression budget
(blueprint: "每批 ship 一次,哪批出问题不影响其它批次").

## B4 decomposition (verified 2026-05-29)

The original blueprint bundled all of B4 ("planner 三分类 + 切 ds-v4-pro + new
schema") into one 2-week batch. Code verification shows it splits along a hard
gate, and that the trace field has two distinct meanings that must not collide:

**Predicted vs realized tier — do not reuse one field for both.**
- `ADR-001` reserves `trace.task_tier` for the planner's **predicted** tier (the
  routing decision). Today there is no producer: `Plan` (`internal/intent/types.go:95`)
  has no tier field; the planner prompt's required-fields list
  (`internal/intent/planner.go:479`) omits it.
- The **realized** tier (which path actually ran) is independently derivable
  *today* from existing trace signals — `planner.cutover_status`
  (`internal/observability/trace.go:189`; `dispatched`→fast, `dispatched_retrieval`→knowledge,
  `fallback_*`/empty→agent), the `retrieval` block (`trace.go:106`, written only on
  the knowledge-QA path), and `tool_calls[].source` (`trace.go:102`, `main_react`
  vs `planner_handler`). These two can diverge (planner predicts fast, turn falls
  through to ReAct = realized agent).
- **Decision (OPEN):** realized tier must go in a **separate** field
  (e.g. `realized_tier` / `dispatch_tier`), leaving `task_tier` for B4b's planner
  output. With both fields, predicted-vs-realized divergence measures planner→
  dispatch **self-consistency / fallback rate** — *not* the `ADR-001:71` <10%
  misclassification gate, which scores predicted tier against **annotator ground
  truth** (realized = the path that actually ran, which itself follows a possibly
  wrong prediction, so it is not ground truth). Reusing `task_tier` for realized
  now and predicted later would break the time series.

**B4a — realized-tier populator (ungated, ship now).**
- Single shared write point: the `Append` path (`internal/observability/trace.go:400`,
  `record.withDefaults(now)`) runs for both CLI and HTTP — derive there, *not* in
  the two duplicated recorders (`cmd/trace.go` `cliTraceRecorder.Finish` and
  `internal/httpapi/trace_recorder.go` `chatTraceRecorder.Finish`). Prefer a
  dedicated `deriveDispatchTier()` step *beside* `withDefaults`, not inside it:
  `withDefaults` only fills default zero-values, and it is exercised on an empty
  record (`trace_test.go:263`) — folding derivation in would fire it on empty
  signals and entangle that test.
- Byte-stable for agent **answers**; **not** byte-stable for trace output (adds a
  JSON key) → requires a `SchemaVersion` bump (B1 set the precedent).
- Feeds the blueprint's open product question #1 (actual fast/knowledge/agent
  ratio), which has been waiting on exactly this data.

**B4b — planner-emitted predicted tier (gated on B2b).**
- New `Plan` field + planner prompt/schema change + N≥20 regression. Gated on B2b
  progressive disclosure shrinking the planner prompt (~6.2k measured / ~5.9k per
  `ADR-004:188`) before the schema change is safe on the cheap model.

## Cross-batch dependencies (verified)

- **Planner model.** The planner runs on `deepseek-v4-flash` *today*
  (`cmd/cli.go:349` → `llm.NewClient(cfg.Agent.LLM)`; `cmd/shared_deps.go:87`
  `IntentPlannerModel = cfg.Agent.LLM.Model`; pinned by `cmd/server_test.go:212`).
  There is **no separate planner-model knob** — it is structurally tied to the base
  model. `ADR-002:99` says the planner *must* be on `ds-v4-pro` until ADR-004
  progressive disclosure lands; the shipped demo stack diverges from that today.
  - **pro is the safe state; flash is the gated cost-down.** Moving the planner
    *to* pro reduces avalanche risk and is **not** gated on progressive disclosure
    (pro handles the big prompt). What is gated is moving it *down* to flash
    cheaply (needs the ~2–3k prompt from ADR-004/B2b).
  - Therefore routing `cmd/cli.go:349` through `For(TierFast)` (which == flash)
    would perpetuate the risky state the blueprint's B4 ("切 ds-v4-pro") wants
    fixed, and would couple the planner model to the fast tier permanently. If the
    planner is migrated through the router, map it to a planner-appropriate tier so
    its model stays independently settable.
- **B2b is the critical path.** ADR-004 progressive disclosure (delivered in B2b)
  is the long pole that unblocks B4b and any planner cost-down.
- **B3 is independent** of B4 (disjoint surface: `internal/renderer` vs
  `internal/intent/planner`) and parallelizable with B2b.
- **SharedDeps.Router field is not zero-touch.** Adding it trips
  `TestSharedDeps_AuditCoversAllSharedDepFields` and
  `TestSharedDeps_NoMutatingSetterLeakage`
  (`internal/engine/shared_deps_leak_audit_test.go`). It is also the natural
  mechanism for putting the planner on a different model than the main loop — so it
  is *not* pure YAGNI if planner-on-pro becomes a near-term goal.

## Open decisions (need lead sign-off)

1. **Planner model strategy.** Move the planner to pro now (ADR-002 compliance,
   addresses flash jitter, costs more, needs N≥20 regression + either a
   planner-model knob or the Router field) vs stay on flash until B2b makes the
   small-prompt flash path safe. Determines whether the Router field is near-term
   or B6-aligned.
2. **Tier ownership.** `ADR-001` makes planner-emitted tier the first-class
   acceptance gate. A deterministic intent→tier code map (more stable, per memory
   `llm-filter-nondeterministic`) would be a deviation requiring an explicit
   ADR-001 amendment, not a silent reframe.
3. **Realized-tier field name** (see B4 decomposition) — confirm `realized_tier` /
   `dispatch_tier` vs another name before B4a ships.

## Refs

- Design: `docs/adr/001`–`007`
- B2a spec: `docs/plans/2026-05-29-b2-router-inject-completion.md`
- BillingAnomaly→fast: `docs/plans/2026-05-29-billing-anomaly-to-fast-tier.md`
- Source breakdown (memory, superseded by this file for status): `architecture-refactor-blueprint-2026-05-28`
