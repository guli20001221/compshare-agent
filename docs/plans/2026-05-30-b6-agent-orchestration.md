# B6 — Agent-Path Orchestration: in-request Saga + in-memory HITL + step-trace (zero DDL)

**Date**: 2026-05-30
**Driver**: ADR-006 (agent-path 6 hard requirements) on top of ADR-001 (agent tier) / ADR-002 (strong-model routing) / ADR-003 (skill ⊥ tool) / ADR-007 (framework anti-pattern).
**Audience**: implementer (Claude/Codex) + reviewer
**Estimated effort**: ~1150 LOC net-new (SSH's ~400 LOC deferred out — §7; **zero DDL**), phased into B6.1 → B6.2 → B6.6.
**Status**: Draft **rev-6** (2026-05-31, lead slim-down). Roadmap had "no spec listed" for B6; this is that spec. rev-5 had deferred SSH out of B6 (#4); **rev-6 applies the lead's zero-DDL slim**: (1) create/mutating no auto-retry (`maxRetries=0`) — **drop idempotency key + dedup table (former B6.3)**; (2) drop cross-process pause/resume — **reuse the shipped in-memory `ConfirmBroker`, in-request synchronous saga, in-memory reverse compensation, no persistence (former B6.4 deleted)**; (3) concurrency lock is in-memory (`agentpool` per-session `entry.mu`) — drop saga-level MySQL `UNIQUE(session_id,skill_id)` lock; (4) migration 0004 adds NO columns (no saga_id/skill_id/task_tier) — **steps go into `trace_json.steps[]`**; (5) self-evolution = per-skill `.memory.md` sibling files (append-only markdown), NOT SQL — so no `skill_id` column reserved. **Net result: DDL = ZERO** (no 0004, no 0005, no migration). `StepTrace` is define-fresh (NOT a reuse of the 8-field CLI-display-only `workflow.StepEvent`). Prior revs (rev-2 4-lens review `wf_b1807b27`; rev-3 no-SSM platform fact; rev-4/rev-5 SSH defer) remain the history this slim sits on.
**Depends on**: ADR-006 ratification (still `Proposed`); the agent-tier dispatch arm does not exist yet (engine has cutover + ReAct only — `roadmap.md` B-state). **Blocks**: B8 (first end-to-end agent skill `deploy_model`); B9 (skill evolution loop consumes step traces). **Independent of**: B2b/B4b (disjoint surface — planner prompt vs orchestrator runtime).

> Grounded in a full read of the current workflow runtime (`internal/workflow/{types,engine,create_instance}.go`), the engine→workflow bridge (`internal/engine/engine.go:3043-3084`), the trace writer (`internal/observability/trace.go:68`), the shipped in-memory confirm path (`internal/httpapi/confirm_broker.go` + `ConfirmCSAgentAction` `dispatch.go:36` + SSE `event: confirmation` `handlers_chat.go:353`), the per-session lock (`internal/agentpool/pool.go:55` `entry.mu`), and the migration baseline (`deploy/migrations/0001-0003` — B6 adds **none**). Every "current" claim carries file:line. ADR-006 fixed the 6 *decisions* + interfaces; this spec adds the **phasing, sequencing, current-state gap, and build order** the ADR does not.

---

## 0. Preconditions (binary — resolve before the gated sub-phases)

1. **Ratify ADR-006** (still `Proposed`, `006:3`). B6.1 hard-codes ADR-006's `StepTrace` schema (`006:34-48`) and the `Step.Compensate`/`Timeout` field names (`006:63-79,89`). A later schema-key change is a rewrite, so ratify the interfaces (or record an explicit start-before-ratify deviation in the PR, not an inline hatch — same discipline as B2b §0).

> **DDL 归零(lead 2026-05-30):** no 0004, no 0005, no migration at all. B6 does not touch DB table structure. `StepTrace` serializes into the existing `trace_json` (the per-turn `agent_traces` row), so there is no DDL-ordering precondition and no SSH-platform-API precondition. The two struck preconditions (former §0.2 SSH platform API, former §0.3 MySQL DDL ordering) are gone.

---

## 1. Why phased (B6.1 → B6.6), and what's parallelizable

B6 is ~1150 LOC of net-new orchestrator + workflow + observability code (`006:252` minus SSH's ~400 LOC deferred out, §7; **zero DDL**, lead 2026-05-30). Shipping it as one PR is unbisectable and un-reviewable. Single-variable sub-phases (memory `phase-a-strict-zero-behavior`, `smoke-test-cannot-infer-architectural-decision`), each independently mergeable with its own CLI/HTTP smoke:

| Sub-phase | Scope | New behavior | Gated on |
|---|---|---|---|
| **B6.1** | Step model + step-trace foundation: extend `workflow.Step` (Compensate/Timeout — **declared, not consumed; no `IdempotencyKey`**); define new `observability.StepTrace` (NOT a reuse of `workflow.StepEvent`) serialized into `trace_json.steps[]`; `Writer.EmitStep` (read-modify-write the turn row); **zero DDL** | **none** (fields nil, `EmitStep` unused — half-wired, like B2b P1) | ADR-006 ratify |
| **B6.2** | Agent-tier step runner: `internal/orchestrator/{step,saga,loop,hitl}.go`; consumes the extended Step; in-request synchronous run; **stop + report on failure (NO reverse compensation — deferred per §5 Amendment); hard-refuses L2 in any step/compensate**; `Step.Timeout`; agent-tier confirm reuses the shipped in-memory `ConfirmBroker`; per-step SSE `event: step`; step audit into `trace_json.steps[]`; **strong-model wiring (#5)** | **yes** — agent-tier run path (in-request sync confirm) | B6.1 |
| **B6.6** | B6 acceptance demo: CreateInstance-as-saga exercising **4 of the 6** requirements (#1/#3/#5/#6; #2 compensation + #4 SSH deferred, see §5 Amendment + §8) | — | B6.2 |

**Deleted sub-phases (lead 2026-05-30):** ~~B6.3 (idempotency)~~ — create/mutating no auto-retry (`maxRetries=0`), no idempotency key / dedup table; ~~B6.4 (async HITL)~~ — no cross-process pause/resume, reuse in-memory `ConfirmBroker` + in-request synchronous saga; ~~B6.5 (SSH)~~ — deferred out of B6 (§7).

**Dependency graph:** B6.1 → B6.2 → B6.6. **The spine is B6.1 → B6.2 → B6.6** (step-trace + in-request synchronous saga + in-memory compensation + strong model + demo). B6.6 closes the batch.

**B6.1/B6.2 are the spine.** B6 = saga (in-request, synchronous) + in-memory HITL confirm + step-trace over API-only workflows (e.g. the `deploy_model` create-saga demo); no idempotency dedup, no async pause/resume, no SSH.

---

## 2. Current state (verified, file:line)

- **`workflow.Step`** (`internal/workflow/types.go:14-21`): `Name, Type, Tool, ToolFunc, BuildArgs, CheckResult`. **No** `Compensate` or `Timeout`. `StepType` is `{StepToolCall, StepConfirm}` (`:6-11`).
- **`workflow.StepEvent`** (`internal/workflow/types.go:71-80`): **8 fields** (`StepName, StepIndex, Total, Type, Status, Tool, Args, Message`), **no json tags**, comment "emitted during workflow execution for UI/CLI display" — i.e. **CLI-display-only**. So ADR-006 #1's `StepTrace` (13 json-tagged persist fields) is **define-fresh, NOT a reuse** of `StepEvent`; the field sets differ.
- **`workflow.Engine.Run`** (`internal/workflow/engine.go:24-107`): synchronous, **fail-STOP** — on `BuildArgs` error / executor error / `CheckResult` false / confirm-decline it sets `result.StoppedAt` and `return`s with **no compensation of already-executed side-effecting steps** (`:46-97`). This is the gap ADR-006 #2 fills. `emit()` (`:109-116`) fires `onStep(StepEvent)` — the CLI-display per-step hook.
- **`ConfirmFunc`** is synchronous (`types.go:68`, `engine.go:103`). The engine bridges it per-workflow-run: `workflow.NewEngine(executor, wfConfirm, onStep)` then `wfEngine.Run(ctx, wf, args)` (`internal/engine/engine.go:3043-3084`), where `wfConfirm` wraps `e.confirmFn`. The HTTP path injects a per-turn SSE-channel-blocking `ConfirmFunc` (`engine.go:881-890,146-149`) — it **blocks the request goroutine** until the user clicks. **This is the path B6 reuses**: the shipped in-memory `ConfirmBroker` (`internal/httpapi/confirm_broker.go` + `ConfirmCSAgentAction` `dispatch.go:36` + SSE `event: confirmation` `handlers_chat.go:353`) already does single-step async confirm within one live request. B6's saga runs in-request and uses this broker — **no cross-process park/resume** (lead 2026-05-30).
- **Per-session concurrency is already serialized**: `internal/agentpool/pool.go:55` — `entry.mu sync.Mutex // serializes per-session engine access`. So same-session concurrent requests are mutually exclusive **in memory**; B6 needs **no** saga-level MySQL `UNIQUE(session_id, skill_id)` lock.
- **`CreateInstanceWorkflow`** (`create_instance.go:156-170`): the 7-step `Definition` (查询镜像→可用配比→库存→价格→**确认**→**创建实例**→查看状态). The side-effecting step is `stepCreateInstance` (`:345`, `CreateCompShareInstance`) — the one needing a `Compensate` (delete-on-rollback). The confirm gate is `stepConfirmCreate` (`:321`, `StepConfirm`). This workflow is B6.6's natural saga demo. **create is no-auto-retry (`maxRetries=0`)** — failure returns to `ConfirmFunc` for re-confirm, no idempotency key.
- **A second runner exists**: `diagEngine.Run` (`engine.go:3184`) runs diagnosis chains the same way — B5 (diagnosis package) will consume B6's step-trace.
- **Trace is per-turn**: `observability.Writer` (`trace.go:68`) with `Append`/`Enqueue`; no step granularity. **Redaction is already centralized** (do NOT re-fix it): `prepareForPersist` (`trace.go:442`) is the single sink-agnostic choke-point both sinks run — `FileWriter.Append` (`trace.go:492`) AND `MySQLWriter.Enqueue` (`mysql_writer.go:124`); the historical MySQL-bypass is closed (`trace.go:434-441` is the past-tense fix narrative; guarded by `mysql_writer_test.go:189`). BUT it only redacts the **query subtree** (`RedactQueryDerivedFields` `trace.go:416-425` = QueryRaw/Normalized/Expansions) — it does **not** touch `StepTrace.Args`/`Result`. So B6.1 must **add a step-trace field redactor INTO `prepareForPersist`**, not reuse the query one (memory `sanitization-covers-all-derived-fields`).
- **Per-turn SSE only**: CLAUDE.md — "ReAct intermediate `StepEvent`s are not exposed in phase 1." So per-step `event: step` UI streaming (ADR-006 §决策1, `006:51,53`) is **net-new** (scheduled in B6.2).
- **Migrations**: `0001_init.sql`, `0002_create_agent_traces.sql`, `0003_add_session_context_version.sql`. **B6 adds NONE** (zero DDL): `StepTrace` serializes into the existing `trace_json` (`steps[]`), not into new columns or tables.
- **STS / strong model already exist**: AssumeRole lives in the `internal/config` STS block + `internal/httpapi` handlers (`006:151`); the per-tier `llm.Router` shipped in B1 (`roadmap.md:19`). B6 *consumes* both — it builds neither.
- **No `internal/orchestrator/`** (greenfield). `Plan` has no `Skills` field (B2b §2, still true); the engine has **no agent-tier dispatch arm** — only Phase-1 cutover + ReAct (`roadmap.md` B-state, `engine.go:1355-1356` capability dispatch). B6 adds the saga runner; **B8 adds the dispatch arm that routes an agent-tier skill into it** (see §7 boundary).

---

## 3. Core architecture decision: extend the shared `Step`, add a new saga runner — do NOT fold saga into `workflow.Engine`

ADR-006 specifies `internal/orchestrator/{step,saga,hitl,loop}.go` (`006:262`) AND extending `workflow.Step` (`006:63`). The resulting two-runner shape (resolved here, recommend lead confirm — §10 Decision A):

- **`workflow.Step` is the shared step type** — extended with `Compensate *CompensateStep` and `Timeout time.Duration` (both **zero-value = current behavior**: nil compensate = no rollback, 0 timeout = inherit ctx). **No `IdempotencyKey` field** (create/mutating is no-auto-retry, so no dedup infra). Existing `workflow.Engine.Run` ignores the new fields → byte-identical for the current CLI create/diagnosis flows.
- **`workflow.Engine.Run` stays** the synchronous, fail-stop runner for **fast/simple flows** (current CreateInstance via CLI, diagnosis chains). It does **not** grow saga semantics — keeping it small honors ADR-007 (no framework creep) and avoids regressing the shipped path.
- **`internal/orchestrator.Saga` is the new agent-tier runner** — consumes the **same** `*workflow.Definition`/`[]workflow.Step`, but adds: success-stack + reverse **in-memory** compensation, `Step.Timeout` enforcement, `StepTrace` emission into `trace_json.steps[]`, and in-request HITL confirm via the in-memory `ConfirmBroker`. The agent dispatch arm (B8) routes agent-tier skills here; everything else stays on `workflow.Engine`. **No persistence, no async resume, no idempotency replay.**

Why not fold saga into `workflow.Engine.Run`: (a) it would add HITL/persistence/compensation cost + cognitive load to the shipped sync path used by the demo-stable CLI create flow (regression surface); (b) ADR-007 anti-framework — the sync runner should stay ~110 lines; (c) single-variable phasing — B6.2 ships the saga runner without touching `workflow.Engine.Run` at all (only the shared `Step` type gains nil-default fields in B6.1).

**Consequence**: `workflow.Step` is the contract seam between the two runners. B6.1 extends it; both runners compile against it; only the orchestrator consumes the new fields.

---

## 4. B6.1 — Step model + step-trace (zero behavior change)

Foundation phase, mirrors B2b P1's "machinery, no consumer" discipline.

- **Extend `workflow.Step`** (`types.go:14`) with the two ADR-006 fields (`Compensate *CompensateStep`, `Timeout time.Duration`) + `CompensateStep` type (`006:74-79`). **No `IdempotencyKey`.** All zero-value-inert. Add a **half-wired guard test**: `workflow.Engine.Run` must NOT read them (assert a Step with a non-nil `Compensate` runs identically — the compensate never fires on the sync runner). This pins the "declared, not consumed" invariant (memory `half-wired-schema-field-grep-whole-chain`, like B2b's ADR-008 evolution fields).
- **`internal/observability/step_trace.go`** (new) — **define a new `StepTrace` struct** (`006:34-48`; NOT a reuse of the 8-field no-json-tag CLI-display-only `workflow.StepEvent` `types.go:71-80` — different field set) + `StepState` enum (`pending/running/awaiting_confirm/success/failed/compensated/**timeout**`, `006:42`; the `timeout` state keeps step-timeout from collapsing into `failed`; `ErrorCategory` is a free string). `StepTrace` serializes into the existing `trace_json` as `Steps []StepTrace json:"steps,omitempty"` (hung on `observability.TraceRecord`). Add **`EmitStep(StepTrace)` to `observability.Writer`** (`trace.go:68`); `EmitTurn`/`Append`/`Enqueue` unchanged (additive interface method — every existing `Writer` impl gets a no-op or real `EmitStep`; assert the interface still satisfied). **`EmitStep` = read-modify-write that turn's row's `steps[]` — NEVER a per-step INSERT** (a per-step INSERT would collide `uk_request_uuid`, one row per turn).
- **Sanitizer choke-point**: ADD a step-trace field redactor (covering `StepTrace.Args`/`Result`) **into the existing `prepareForPersist`** (`trace.go:442`) — which both the file sink AND the MySQL `Enqueue` worker already run (`trace.go:492`, `mysql_writer.go:124`), so coverage is automatic on both. Do **NOT** reuse `RedactQueryDerivedFields` (`trace.go:416-425`): it redacts only the query subtree and is a no-op for the new fields. One choke function (memory `sanitization-covers-all-derived-fields`); there is no existing MySQL bypass to close (B4a already centralized it).
- **Zero DDL** (lead 2026-05-30): **no migration 0004, no ALTER, no new columns** (`step_id`/`saga_id`/`skill_id`/`task_tier` all dropped). Steps go entirely into `trace_json.steps[]`; the existing `agent_traces` schema + `uk_request_uuid` are untouched. (Realized-tier already shipped in B4a; predicted `task_tier` is B4b's own concern and does **not** require a B6 column — §10 Decision D.)
- **Acceptance**: `go test ./...` green; `workflow.Engine.Run` byte-identical (half-wired guard); `EmitStep` exists + is a no-op everywhere (no producer yet); **no migration added** (`deploy/migrations` unchanged). **No StepTrace is actually emitted in B6.1** (no saga runner) — this is types+schema only (memory `type-contracts-visibility`).

---

## 5. B6.2 — In-request agent-tier step runner + HITL + strong model + step audit

The spine. `internal/orchestrator/` is born here (~600 LOC after the compensation-deferral amendment; was ~800).

> **Amendment(lead 2026-05-31,ADR-006 §决策2 Amendment 镜像)**:**实例创建不自动回滚 → 倒序补偿执行 deferred**。lead 复核:create 失败不计费(无副作用)、create 成功是用户资源(后续步失败不该自动删,删=L2 `TerminateCompShareInstance`=永不自动执行)、agent 行为经 `trace_json.steps[]`+context 完全可知由用户决定。**后果**:B6.2 交付 **4 项**(#1 step-trace / #3 HITL / #5 强模型 / #6 create 不重试),**不**实现 `runCompensation` 倒序执行 / `executedStep` 成功栈 / create-step `Compensate`;`Step.Compensate`(B6.1)维持 declared-not-consumed。新增**硬安全规则**:saga 在构造期 `security.Check` 拒任何 forward 步(及将来 compensate)的 L2/destructive 动作 → 结构上杜绝"自动删实例",**无需任何 L2-bypass**(原计划要给 compensate 开 ExecuteSafe:189 的口子,本 amendment 取消)。失败语义 = **stop + report**(像 `workflow.Engine.Run`,但带 step-trace + timeout + HITL + 强模型 + agent-tier 接线)。下面 saga.go/compensation 描述保留为将来补偿落地蓝图,**B6.2 不实现执行**。

- **`step.go`** — executes one `workflow.Step` with `Timeout` enforcement (`006:89`, default **240s = 4 min**, per-step override; >4min override = build-time warning so widening isn't silent), emits `StepTrace` at each transition (incl. the `timeout` state on step-timeout), computes/records the result for the success-stack.
- **`saga.go`** — `Run(ctx, def, params, confirm ConfirmFunc)`: forward pass building `executedSteps []executedStep`; on any failure invokes `runCompensation(ctx, executedSteps)` — **reverse, in-memory** iteration calling each `Step.Compensate` (`006:82`). Compensate emits `StepTrace{state:compensated, compensate_of:<id>}`. `BestEffort` (`006:77`) **defaults true** (`006:95`): a failed compensate logs + continues the rollback rather than wedging ("partial rollback + tell user to check console" > "rollback deadlock"). The whole saga runs **in one live request** — no persistence, no cross-process resume. **Hard rule (ADR-006 `006:96`): irreversible ops (Delete/Terminate) must NOT carry a Compensate** — they are blocked at the confirm gate, never entered into the saga forward pass. Add a test asserting no saga step's Tool is an L2/always-refuse action (`security/levels.go`).
- **`loop.go`** — the agent-tier ReAct/skill driver shell (minimal; the LLM-in-the-loop reasoning is B8's skill body, not B6 infra — keep this thin per ADR-007).
- **`hitl.go`** — agent-tier confirm **reuses the shipped in-memory `ConfirmBroker`** (`internal/httpapi/confirm_broker.go` + `ConfirmCSAgentAction` `dispatch.go:36` + SSE `event: confirmation` `handlers_chat.go:353`), which already does single-step async confirm **within one live request**: server SSE-emits `confirmation` → frontend POSTs `ConfirmCSAgentAction` → broker releases the blocked goroutine. CLI uses the existing synchronous `cmd/agent.go::cliConfirm`. **No `HITLStore` interface, no `PauseToken`, no `saga_pauses` table, no `ResumeSaga` Action** — those are deleted (lead 2026-05-30); the saga never parks across processes.
- **Per-step SSE `event: step`** (ADR-006 §决策1, `006:51,53` — "UI 通过 SSE channel 实时收"): the saga fans each `StepTrace` transition to BOTH the `Writer` (persist into `trace_json.steps[]` via B6.1's `EmitStep` read-modify-write) AND, on the HTTP path, the SSE stream as `event: step` for live UI. **Net-new** (CLAUDE.md: phase-1 HTTP does not expose intermediate `StepEvent`s); CLI keeps the inline `onStep` print (`engine.go:3048-3082`).
- **Strong model (ADR-006 #5, `006:219-223`)**: any LLM call on the saga path goes through `router.For(TierAgent)` (ADR-002). Acceptance = `grep r.For(TierAgent) ≥ 1`. Folded here (trivial, no separate sub-phase). The `TierAgent` strong-model pick is grep-gated.
- **Timeline invariant** (`006:84-89`): two-layer `180s (LLM timeout, ADR-002) < 240s (Step.Timeout)`. Encode as a build-time assertion or a documented constant ordering so a future config edit can't invert it. (No `PauseToken` layer — there is no cross-process pause.)
- **Demo wiring(amended)**: run `CreateInstanceWorkflow` through the orchestrator (step-trace + HITL confirm at `stepConfirmCreate` + strong-model + per-step timeout). **`stepCreateInstance` carries NO `Compensate`** (per §5 Amendment — created instance is not auto-deleted). **create is `maxRetries=0`** (already in `policies.go:131-138`) — a failed create returns to confirm, never auto-retries. **E2E tests**: ① create-fail (executor error) → no retry, saga stops + returns to confirm, no compensate; ② a step's Tool that is L2 (`security.Check`) is rejected at saga construction; ③ a forced per-step timeout emits `StepTrace{state:timeout}`.
- **Acceptance(amended)**: `internal/orchestrator` compiles + tests **standalone** (in-memory confirm, no persistence); orchestrator runs CreateInstanceWorkflow end-to-end (in-request confirm); failure → **stop + report** (no auto-rollback; created instance, if any, surfaced via `trace_json.steps[]`); `StepTrace` emitted per step into `trace_json.steps[]` (read-modify-write of the turn record, **never per-step INSERT**), AND surfaced as SSE `event: step` on the HTTP path (infra exists, `handlers_chat.go:330`); `router.For(TierAgent)` grep ≥1; saga hard-refuses L2 in any step (`security.Check`); `Step.Timeout` enforced (timeout state); no migration added.

---

## 6. Deleted sub-phases: ~~B6.3 (idempotency)~~ + ~~B6.4 (async HITL)~~ (lead 2026-05-30)

Both former sub-phases are **deleted** by the zero-DDL slim. They are recorded here only so a reader of the old roadmap doesn't think they were forgotten.

**~~B6.3 — Idempotency~~ → replaced by "create no auto-retry"** (ADR-006 §决策6 rewrite): create/mutating actions run with **`maxRetries=0`**; on failure they return to `ConfirmFunc` for re-confirm. Double-creation only comes from retries, so with no retry there is **no idempotency-key need** — `Step.IdempotencyKey`, the `(session_id, saga_id, step_id, idem_key, result)` dedup table, and the cached-result replay are all dropped.
> **待核实(实现时)**:`SafeToolExecutor` 重试是否按 action 类别可配。若一刀切,需给 mutating/create 类加 `maxRetries=0` 分类(Go 改动)。reviewer 未核此点。

**~~B6.4 — Async HITL~~ → replaced by in-memory `ConfirmBroker` + in-request synchronous saga** (ADR-006 §决策3 rewrite): no cross-process pause/resume. The shipped in-memory `ConfirmBroker` (`confirm_broker.go` + `ConfirmCSAgentAction` + SSE `event: confirmation`) already does single-step async confirm within one live request; the saga runs to completion in-request and compensates in memory on failure. **Dropped**: `PauseToken` / `Decision` / `HITLStore` interface / `saga_pauses` table / `event: requires_action` / `Action=ResumeSaga` / expiry-GC / cross-replica sticky-session-for-resume. Same-session concurrency is already serialized by `agentpool` `entry.mu` (`pool.go:55`) — **no saga-level MySQL `UNIQUE(session_id, skill_id)` lock**. `ConfirmFunc` stays as the sync CLI/fast-path confirm; the HTTP per-turn `ConfirmFunc` (`engine.go:881-890`) is the in-request broker path the saga reuses.

**Net: DDL = ZERO** — no migration 0004, no 0005, no `saga_pauses`. The spine is **B6.1 → B6.2 → B6.6**.

---

## 7. SSH diagnostics: **DEFERRED out of B6** (lead, 2026-05-30)

**Parked — not in B6 scope.** The lead deferred all SSH-involving diagnosis: the design isn't settled and isn't needed for B6's spine. Two candidate approaches are **recorded for a future decision (do NOT pick now)**:
- **(a) A dedicated Go-side diagnosis skill that SSHes in** — our own SSH execution behind a diagnosis skill (the credential + structural-allowlist work the original ADR-006 §决策4 envisioned, minus the unavailable SSM API).
- **(b) Claude Code as a constrained sub-agent that SSHes in** — reuse Claude Code's agent loop + `PreToolUse`-hook permission system, on **ds-v4** via the Anthropic route. **ModelVerse ds-v4 is natively Anthropic-format-compatible** (lead-confirmed 2026-05-30), so the routing is clean — no compat shim needed.

**Fixed constraints carried to the future design** (so it starts grounded): no platform SSM-style command-exec API — **direct SSH only**; explicit user **consent** required; **only whitelisted** read-only commands (a structural argv allowlist, deny-by-default — the 12-cmd table that used to live in ADR-006 §决策4 moves into whichever future batch builds SSH); credential must be **scoped/ephemeral**, never a stored persistent key; the shipped boundary `internal/prompt/segment_readonly.go:11` ("助手不能 SSH 登录实例") must be amended whichever way it goes. The winning approach triggers an **ADR-006 §决策4 amendment** + the `segment_readonly.go` edit (+ an ADR-002 note iff the Claude-Code/ds-v4 route is chosen).

**Consequence for B6**: the spine is **B6.1 → B6.2 → B6.6** (step-trace into `trace_json` + in-request synchronous run + in-memory `ConfirmBroker` HITL + strong model). B6 delivers **4 of ADR-006's 6 requirements** — #1 step-trace / #3 HITL / #5 strong-model / #6 (create no-auto-retry, replacing idempotency); **#2 compensation (§5 Amendment) and #4 (SSH) are deferred** to future batches. This removes B6's biggest uncertainties (auto-rollback of irreversible ops; credential / sandbox / external-binary) from the critical path.

---

## 8. B6.6 — Acceptance demo: the B6/B8 boundary

ADR-006 acceptance (`006:268`) wants "1 end-to-end skill exercising all 6 requirements." This creates a B6/B8 overlap that must be drawn explicitly (§10 Decision C):

- **B6's demo is INFRA-complete, not product-complete**: run `CreateInstanceWorkflow` (existing 7 steps) through the **orchestrator** with an in-request in-memory `ConfirmBroker` confirm + step traces (into `trace_json.steps[]`) + strong-model + per-step timeout. This exercises requirements **#1/#3/#5 + #6-as-no-retry** end-to-end **without** authoring new product reasoning. **#2 compensation is deferred** (§5 Amendment — created instance not auto-deleted) and **#4 (SSH) is deferred** (§7) — both out of the B6 demo.
- **B8 is product-complete**: the full `deploy_model` skill — model-name→VRAM→GPU reasoning (the #1 deploy gap, needs the deterministic `GetModelVRAMRequirement` tool), image selection, and the agent-tier **dispatch arm** that routes "我想部署 Qwen32B" into the saga. B6 provides the runway (saga API + the dispatch hook signature); B8 builds the plane.
- **Why this split**: B6 must be mergeable + testable without waiting on the VRAM-reasoning design; conflating them makes B6 un-shippable (memory `roadmap-item-granularity`). The `deploy_model/skill.md` draft (ADR-004's headline skill example) is B8 scope.

---

## 9. Test plan

1. `go test ./...` green through every sub-phase (B6.1 byte-identical for `workflow.Engine.Run`).
2. B6.1: half-wired guard (sync runner ignores new Step fields); `EmitStep` interface-satisfaction + no-op; `EmitStep` does read-modify-write of the turn row's `trace_json.steps[]`, **never a per-step INSERT** (assert no second row / no `uk_request_uuid` collision); the new step-trace redactor scrubs `Args`/`Result` through `prepareForPersist` on **both** sinks (regression test mirroring `mysql_writer_test.go:189` with a step-trace PII fixture); **no migration added** (`deploy/migrations` unchanged).
3. B6.2: `internal/orchestrator` **compiles + tests standalone** (in-memory confirm, no persistence); **saga E2E** — CreateInstanceWorkflow with injected post-create failure → assert reverse in-memory compensation deletes the created instance (`006:98`); `StepTrace` sequence assertion (incl. `timeout` state on a forced step-timeout); per-step `event: step` emitted on the HTTP saga path; irreversible-op-not-in-saga guard (no L2 Tool in any saga step); create-failure → no auto-retry (`maxRetries=0`), returns to confirm.
4. ~~B6.3 idempotency tests~~ — **deleted** (no idempotency key / dedup; create is no-auto-retry).
5. ~~B6.4 async-HITL tests~~ — **deleted** (no pause/resume / `saga_pauses` / saga-level lock). Same-session concurrency is covered by the existing `agentpool` `entry.mu` serialization, not a new lock.
6. ~~B6.5 SSH tests~~ — **deferred with SSH** (§7); not in B6.
7. **CLI + HTTP smoke** (memory `cli-regression-for-capability-changes`, `offline-eval-not-equal-cli-smoke`): the saga demo through the real binary on both paths; HTTP in-request confirm via the `ConfirmBroker` (no reconnect/resume to test — saga completes in one request).

---

## 10. Open decisions (lead sign-off)

- **A. Two-runner shape (§3)** — extend shared `workflow.Step` + new `internal/orchestrator.Saga`, leaving `workflow.Engine.Run` as the sync runner. **Recommend YES** (ADR-007 anti-framework + zero regression to the shipped CLI create path). Alternative (fold saga into `workflow.Engine`) rejected — grows the shipped sync runner.

> Former decisions **B (SSH defer), C (demo scope), D (task_tier column), E (ADR-002 ordering)** are resolved/removed by the lead's slim-down: SSH is deferred (§7, settled); the demo is fixed as CreateInstance-as-saga (§8); there is **no `task_tier` column** (zero DDL — B4b's predicted-tier is its own concern, no B6 slot); the ADR-002 planner-model amendment is tracked separately and does not gate B6's `TierAgent` grep.

---

## 11. Risks

- **Orchestrator-as-framework creep** (ADR-007, `006:256`): cap `internal/orchestrator` at ~800 LOC for exactly the requirements; no generic graph/DAG/plugin abstraction. Reviewer gate: any "engine"/"registry"/"plugin" abstraction beyond the 4 files is a smell.
- **SSH diagnostics deferred out of B6** (§7, lead): removes B6's biggest uncertainty (credential / sandbox / external-binary) from the critical path. B6 ships **4 of 6** ADR-006 requirements (#1/#3/#5/#6); **#2 compensation (§5 Amendment) + #4 (SSH)** become future batches with the candidate approaches + fixed constraints recorded.
- **Compensate correctness** (`006:95`): BestEffort-default-true + irreversible-not-in-saga; E2E test the rollback (in-memory, in-request), don't trust the happy path.
- **No persistence safety net**: the saga is in-request only — a server crash mid-saga loses in-flight state (no `saga_pauses` to rehydrate). Accepted tradeoff (lead 2026-05-30): the create demo is short-lived; compensation runs in-memory before the request returns. If a future skill needs long-running durable sagas, that re-opens the persistence question (out of B6).
- **StepTrace PII**: `prepareForPersist` (`trace.go:442`) already redacts turn traces on **both** sinks, but only the query subtree (`RedactQueryDerivedFields`). B6.1 must EXTEND it with a step-trace field redactor (Args/Result) — extend coverage, **no existing bypass to close** (the B4a MySQL leak is already centralized + guarded). Missing this re-introduces a leak via the new fields, not via the sink.
- **`EmitStep` row-collision**: `EmitStep` must read-modify-write the turn's existing `trace_json.steps[]`, **never per-step INSERT** — a per-step INSERT collides `uk_request_uuid` (one row per turn). Test guards this.
- **Scope** (`006:252`, now ~1150 LOC with SSH deferred + zero DDL): the phasing (§1) is the mitigation — each sub-phase ships + smokes independently; B6.1 (types + step-trace) delivers value even if B6.2 slips.

---

## 11b. Future-batch constraint (B9 self-evolution, NOT B6 scope)

- **Self-evolution via files, not SQL** (MUSE 2605.27366): skill-level memory = per-skill `.memory.md` sibling files (append-only markdown), never a DB table — which is another reason B6 reserves **no `skill_id` column**.
- **MUSE hardcoded-identifier risk** (hvac-control regression 80%→20%, MUSE §4.5 case iv): a self-evolving skill bakes single-run calibration constants / paths / numeric ranges into its body. **Before accepting a refined skill, B9 MUST sanitize hardcoded identifiers** (project_id / instance_id / region / RetCode / quota numbers) — same family as the compshare V100S RetCode=230 case. **This is a B9 constraint, recorded here so the orchestrator's step-trace consumer (B9) starts grounded; it is not B6 work.**

---

## 12. Refs

- ADR-006 (the 6 decisions + interfaces; SSH allowlist deferred with #4), ADR-001 (agent tier), ADR-002 (TierAgent router), ADR-003 (skill ⊥ tool), ADR-007 (framework anti-pattern).
- Current runtime: `internal/workflow/{types.go:14,types.go:71-80,engine.go:24,create_instance.go:156}`, in-memory confirm `internal/httpapi/{confirm_broker.go,dispatch.go:36,handlers_chat.go:353}`, per-session lock `internal/agentpool/pool.go:55`, bridge `internal/engine/engine.go:3043-3084`, writer `internal/observability/trace.go:68`, migrations `deploy/migrations/0001-0003` (B6 adds **none**).
- Roadmap `docs/plans/roadmap.md` (B6 row + B4 realized/predicted-tier separation).
- B8 consumer: `deploy_model` skill (ADR-004 headline example) + `GetModelVRAMRequirement` (the #1 deploy gap — model→VRAM→GPU reasoning, deterministic tool).
- Memory: `no-graph-framework-for-agent`, `l0-stop-grow-dictionary`, `phase-a-strict-zero-behavior`, `sanitization-covers-all-derived-fields`, `type-contracts-visibility`, `half-wired-schema-field-grep-whole-chain`, `cli-regression-for-capability-changes`, `roadmap-item-granularity`, `internal/security is secret boundary, not product policy`, `package-boundary-security-vs-policy`.
