# Architecture Refactor Roadmap (B1–B9)

**Status of record.** This file is the version-controlled batch roadmap for the
task-tier architecture refactor. It records *what ships in which batch, in what
order, and what blocks what* — the sequencing layer. It does **not** restate the
design: the canonical design lives in `docs/adr/001`–`008`. When a batch lands,
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
| **B2b** | Skill/Tool directory split + codegen + **progressive disclosure** (planner prompt → metadata-only) | 003, 004, 008 | 🟡 spec rev-3, **§12 gate CLOSED 2026-05-30** (ADR-003/004 provisionally accepted; revert-if-regression) — impl pending, lead deferred start to post-compact | `docs/plans/2026-05-29-b2b-skill-tool-dir-codegen.md` §12 |
| **B3** | Fast-tier catalog envelopes (gpu_specs/stock/image) render via deterministic template; opt-in `USE_GROUNDED_RENDERER=fast_template`, default-off | 001 | ✅ shipped | `a25e19e` (merge `535fc1b`) — **flip-gate** before shipped-config flip: render\*Reply polish (label `性能`, localize `状态`) + stock-template still un-exercised |
| **B4a** | Observability: derive **realized tier** from dispatch path; populate `realized_tier` in the two recorders (NOT `Append` — MySQL bypasses it) | 001 #4 | ✅ shipped | `67a6b84` + `e092444` (merge `a87f5e1`) |
| **B4b** | Planner emits **predicted tier** (new output field + prompt/schema change); N≥20 regression | 001 #4, 002 | ⏳ pending (gated on B2b) | see "B4 decomposition"; **planner stays on flash — Decision #1 empirically ruled out pro (worse on borderlines)** |
| **B5** | Diagnosis package, k8sgpt-style (Analyzer/Failure/Filter) | 005 | ⏳ pending | — |
| **B6** | Agent path infrastructure: orchestrator + saga + multi-step HITL + SSH category sandbox | 006 | ⏳ pending | — |
| **B7** | MCP gateway (in-process + external stdio/HTTP entry) | 003 Amendment 1 | ⏳ pending | — |
| **B8** | First end-to-end agent skill ("deploy Qwen32B" / "SSH debug") | 006 | ⏳ pending | — |
| **B9** | Skill self-evolution loop: revision → held-out validation (B5 verifier) → governance; identifier-sanitize + retire/merge/prune enforcement | 008 | ⏳ pending (gated on B5+B6; B2b only reserves the frontmatter fields) | ADR-008 |

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
  (`internal/observability/trace.go:189`), the `retrieval` block (`trace.go:106`,
  written only on the knowledge-QA path), and `tool_calls[].source`
  (`trace.go:102`, `main_react` vs `planner_handler`). These two can diverge
  (planner predicts fast, turn falls through to ReAct = realized agent).
- **Derivation is priority-ordered, NOT a `cutover_status` prefix sweep.**
  `cutover_status` has 13 values (`internal/intent/handler.go:42-58`) and is set
  *only* on the Phase-1 cutover path, so empty does **not** mean agent (it means
  "not a cutover turn"). The sound mapping (implemented in B4a as
  `TraceRecord.DeriveRealizedTier`): `dispatched_retrieval`→knowledge;
  `dispatched`/`selection_required`→fast; else `retrieval.hits>0`→knowledge; else
  a `main_react` tool fired→agent; else `""` (unknown). `fallback_*` and
  `failure_after_tool` are resolved by what *actually executed* (steps 3–4), not
  by status name. Unobservable turns (no-tool ReAct, hard-block/canned reply) stay
  `""` rather than default-agent, so refusals don't inflate the agent share
  (memory `attribution-observable-only`).
- **Decision (OPEN):** realized tier must go in a **separate** field
  (e.g. `realized_tier` / `dispatch_tier`), leaving `task_tier` for B4b's planner
  output. With both fields, predicted-vs-realized divergence measures planner→
  dispatch **self-consistency / fallback rate** — *not* the `ADR-001:71` <10%
  misclassification gate, which scores predicted tier against **annotator ground
  truth** (realized = the path that actually ran, which itself follows a possibly
  wrong prediction, so it is not ground truth). Reusing `task_tier` for realized
  now and predicted later would break the time series.

**B4a — realized-tier populator (ungated; implemented, in review).**
- Branch `feat/b4a-realized-tier` (commit `67a6b84`). Adds `realized_tier`
  (omitempty) + `TraceRecord.DeriveRealizedTier` + the two recorder wirings +
  `SchemaVersion` v0.3→v0.4; `go test ./...` green.
- **Correction (verified against code):** `FileWriter.Append`
  (`trace.go:398`, runs `withDefaults`) is **not** a choke point for the server
  path. `chatTraceRecorder.Finish` calls `MySQLWriter.Enqueue` *directly*
  (`internal/httpapi/trace_recorder.go:246` → `mysql_writer.go:118`), and the
  worker marshals the record as-is (`mysql_writer.go:234`) — `Append`,
  `withDefaults`, and `RedactQueryDerivedFields` are all bypassed on MySQL. So the
  derivation is set in the **two recorders** (`cliTraceRecorder.Finish`,
  `chatTraceRecorder.Finish`) on the record *before* the writer call, via a shared
  `DeriveRealizedTier` helper. That covers file, mysql, and the both-sink
  `multiTraceWriter` (which only fans out an already-populated record). Not in
  `Append` (misses mysql), not in `withDefaults` (empty-record test
  `trace_test.go:263`).
- Byte-stable for agent **answers**; **not** byte-stable for trace output (adds a
  JSON key when populated) → `SchemaVersion` bumped to `trace.v0.4`. (B1 added
  `task_tier` as a never-populated reserved slot and did *not* bump — omitempty
  kept it byte-identical; B4a bumps because it actually populates.)
- Feeds the blueprint's open product question #1 (actual fast/knowledge/agent
  ratio), which has been waiting on exactly this data.
- **Side-finding (NOT in this PR — pre-existing, needs its own decision):** the
  MySQL sink bypasses `RedactQueryDerivedFields` (`trace.go:401`, its only
  production caller) and `withDefaults`, so server traces persist query-derived
  fields unredacted and with an empty `schema_version`. Flagged for a separate
  fix (privacy-adjacent; memory `sanitization-covers-all-derived-fields`).

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
  - **Decision #1 (resolved 2026-05-29, jitter + pro oracle): stay on flash.**
    The `ADR-002:99` premise "pro is the safe interim state" is **empirically
    contradicted** for the current pre-ADR-004 prompt. A 6-question jitter check
    (N=8 each, planner on each model) found `ds-v4-pro` *worse* than flash on
    borderlines: pro leaks to `unknown` and raises `schema_invalid` where flash
    holds `si=0` (`ssh-boundary`, `vague-monitor`), while the zero-target
    `帮我关机` failure is `schema_invalid` 8/8 on **both** (a prompt/`target_ref`
    gap, not a model gap). So the reliability lever is the **prompt** (B2b
    progressive disclosure + directive cleanup + `target_ref` few-shot), not the
    model. **Action: feed into ADR-002 ratification** — the "must be on pro until
    ADR-004" clause should be revised, not rubber-stamped. Caveat: N=8, current big
    prompt; re-test pro under B2b's smaller prompt before generalizing.
  - Therefore do **not** route the `cmd/cli.go` planner through `For(TierFast)`
    *or* migrate it to pro now — keep it on the base (flash) model. Revisit only if
    a post-B2b re-test flips the result.
- **B2b is the critical path.** ADR-004 progressive disclosure (delivered in B2b)
  unblocks **B4b's planner schema change unconditionally**. Decision #1 settled the
  pro-vs-flash fork to **flash**, and the lead has relaxed the token-cost
  constraint (cheap model) — so **`≤3k` is downgraded from a gate to a reported
  metric**: progressive disclosure's value is now prompt *clarity/maintainability*
  + the reliability **prompt-structure** work (imperative directives, `target_ref`
  few-shot), not hitting a byte target. The avalanche evidence (Decision #1) points
  at prompt *content/structure*, not size.
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
3. **Realized-tier field name** — B4a shipped as `realized_tier` (defaulted by
   the implementer). Rename to `dispatch_tier`/other is a trivial in-review change
   if lead prefers; flagging rather than blocking.

## Refs

- Design: `docs/adr/001`–`008`
- B2a spec: `docs/plans/2026-05-29-b2-router-inject-completion.md`
- BillingAnomaly→fast: `docs/plans/2026-05-29-billing-anomaly-to-fast-tier.md`
- Source breakdown (memory, superseded by this file for status): `architecture-refactor-blueprint-2026-05-28`
