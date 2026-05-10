---
status: draft for review
parent: stage2a-phase1-demo-cutover.md
ticket: T-Hardening.ReadExpensiveGateway
phase: Hardening
created: 2026-05-11
primary_baseline: deepseek-v4-flash
---

# T-Hardening.ReadExpensiveGateway Contract

## Goal

Keep the default user experience on `deepseek-v4-flash` + current ReAct routing while adding deterministic guardrails around read-expensive API usage. This ticket is about API-abuse prevention, not answer style.

The intended outcome:

- Resource / monitor / instance-billing questions continue to use the current ReAct path by default.
- Stage 2 planner and deterministic handlers remain available for shadow / opt-in evaluation only.
- Read-expensive tool calls are quota-controlled, target-capped, time-window-capped, and visible in trace.
- Existing hard-blocks, SecretBoundary, SafeToolExecutor, RateLimiter, and trace remain the security substrate.

## Non-Goals

- No EntityRegistry enforcement / validator promotion.
- No deterministic renderer promotion.
- No hybrid renderer implementation.
- No mixed billing KB handler.
- No prompt-only force-tool fallback.
- No Redis / distributed limiter implementation.
- No destructive or mutating policy change.
- No removal of `isAccountBillingUnsupported`.
- No removal of the existing monitor `NO_DATA_IN_REQUESTED_WINDOW` reactive guard.

## Dependencies

Already present:

- T-001 SafeToolExecutor and ToolExecutionPolicy.
- T-002 SecretBoundary.
- T-004b EntityRegistry runtime, observable / non-binding.
- T-005 RateLimiter / QuotaManager for LLM and mutating calls.
- T-006 trace writer.
- T-007b shadow planner trace.
- Stage 2B curated FAQ retrieval, default-off.

## Current State

The current security substrate is mostly in place, but read-expensive API usage is still under-protected:

- `internal/tools/policies.go` already defines `ActionClassReadCheap`, `ActionClassReadExpensiveDefault`, and `ActionClassReadExpensivePerTarget`.
- `GetCompShareInstanceMonitor` is classified as `read_expensive_per_target`.
- `Diagnose*` and action names containing `price` are classified as `read_expensive_default`.
- `DescribeCompShareInstance` is currently classified as `read_cheap`; this is unsafe because empty `UHostIds` returns the account inventory.
- `internal/engine/engine.go` explicitly skips read classes in rate limiting; only LLM and mutating classes are currently limited.

This ticket reuses the existing policy structure. It does not rewrite the action registry.

## Core Decisions

### 1. Default UX: ReAct + Shadow

The default demo / real-account test path is current ReAct with `deepseek-v4-flash`.

Rules:

- `USE_INTENT_PLANNER=shadow` may remain enabled for observability.
- `USE_INTENT_PLANNER_FOR` must default to empty / unset in examples and manual test instructions.
- Empty `USE_INTENT_PLANNER_FOR` means no deterministic cutover.
- Deterministic cutover remains opt-in for evaluation.
- `resource` / `monitor` cutover must not be treated as the recommended default user experience.

CLI startup observability:

- The CLI startup banner must include one line with:
  - `planner_mode=off|shadow`
  - `cutover_intents=[]` or the explicit enabled set, e.g. `[resource,monitor]`
- Trace v0.2 must also include the same turn-level runtime mode metadata. A banner alone is not sufficient because offline trace analysis cannot rely on terminal output.

`planner_mode` and `cutover_intents` are deliberately separate because the runtime can be `shadow` and cut over selected intents in the same session. Do not collapse this into a single `cutover` mode.

### 2. Quota Layers

Use three independent layers because they control different abuse surfaces:

| Layer | Owner | Scope | Purpose |
| --- | --- | --- | --- |
| Subject quota | `governance.RateLimiter` | per hashed public key / subject | Prevent one account key from sustained API abuse. |
| Turn-scoped budget | `Engine` | per user turn | Prevent one prompt from creating unbounded ReAct loops. |
| Per-call caps | `SafeToolExecutor` | one tool invocation | Prevent oversized target lists or time windows before the API call. |

Subject identity remains the hashed CompShare public key:

- `SubjectKey = sha256(COMPSHARE_PUBLIC_KEY)`.
- Raw key material must never enter logs, trace, limiter state, or error strings.
- If no public key is configured, use `anonymous` and warn at startup.

Phase 1 limitation:

- The process-local `MemoryLimiter` is acceptable for local demo / single-instance deployment.
- It is not a production multi-replica limiter. Multi-replica deployments need a centralized limiter, such as Redis or an API gateway. Add an explicit code comment where the limiter is constructed.

### 3. New Read-Expensive Quota Class

Add a new governance class:

```go
const ClassReadExpensiveTool Class = "read_expensive_tool"
```

Default limits:

| Class | Default QPS | Default daily |
| --- | ---: | ---: |
| `llm` | existing default | existing default |
| `mutating_tool` | existing default | existing default |
| `read_expensive_tool` | 3 | 500 |

The exact defaults may be tuned later from real-account traces, but this ticket must introduce the class and wiring.

Config keys:

```yaml
agent:
  rate_limit:
    read_expensive_qps: 3
    read_expensive_daily: 500
```

`0` or omitted uses defaults. Negative values fail fast with the YAML path in the error, matching the existing `llm_qps` / `mutating_qps` behavior.

Rate-limit denial:

- Reuse `ReasonQPSExceeded` / `ReasonDailyExceeded`.
- Do not add a new reason for target/window caps; those are policy caps, not quota-denial reasons.
- Main ReAct tool calls denied by read-expensive quota should produce a clear tool-result message for the LLM to summarize, not a raw API failure.
- Workflow / diagnosis internal calls denied by read-expensive quota stop the chain and return the same friendly quota message to the user. They should be surfaced as blocked quota decisions, not raw API errors.
- Init-context refresh quota denial is startup-observable only: warn to stderr and write trace when tracing is enabled; do not fail CLI startup solely because the initial registry refresh quota is exhausted.
- Mutating quota behavior from T-005 remains unchanged.

### 4. Action Classification Adjustments

This ticket should make small, explicit changes:

1. Change `DescribeCompShareInstance` from `read_cheap` to `read_expensive_default`.
2. Replace the current `strings.Contains(lower, "price")` classification with an explicit price/billing allowlist.
3. Reuse existing `read_expensive_per_target` for `GetCompShareInstanceMonitor`.
4. Reuse existing `read_expensive_default` for `Diagnose*`.

Do not introduce broad substring classification such as `strings.Contains(action, "Instance")`.

Initial explicit read-expensive action set:

| Action | Class | Notes |
| --- | --- | --- |
| `DescribeCompShareInstance` | `read_expensive_default` | Empty target list returns account inventory. |
| `GetCompShareInstanceMonitor` | `read_expensive_per_target` | Per-target and history-window caps apply. |
| `GetCompShareInstancePrice` | `read_expensive_default` | Price-related API. |
| `GetCompShareInstanceUserPrice` | `read_expensive_default` | Price-related API. |
| `DiagnoseBilling` | `read_expensive_default` | Meta tool; internal calls consume subject quota. |
| `DiagnoseSSH` | `read_expensive_default` | Diagnosis can call APIs internally. |
| `DiagnoseInitFailure` | `read_expensive_default` | Diagnosis can call APIs internally. |
| `DescribeAvailableCompShareInstanceTypes` | `read_expensive_default` | Registered platform/spec API; can return large spec matrices. |
| `CheckCompShareResourceCapacity` | `read_expensive_default` | Registered capacity API; should not be freely spammed. |

`DescribeCompShareSupportZone`, `DescribeCompShareMachineTypeFamilies`, `DescribeCompShareGpuInventory`, and `GetCompShareRestResource` are not currently registered in `internal/tools/registry.go`. If they are registered later, this ticket's policy requires adding them to the explicit read-expensive allowlist in the same PR unless there is a documented reason to keep them read-cheap.

### 5. Per-Call Caps

Add optional policy fields:

```go
MaxTargetsPerCall       int
MaxHistoryWindowSeconds int
```

Initial defaults:

| Action | MaxTargetsPerCall | MaxHistoryWindowSeconds | Behavior |
| --- | ---: | ---: | --- |
| `DescribeCompShareInstance` | 0 | 0 | No hard target cap in v1; quota still applies. |
| `GetCompShareInstanceMonitor` | 20 | 86400 | Reject if too many targets or history window exceeds 24h. |
| `GetCompShareInstancePrice` | 20 if target args exist | 0 | Reject oversized explicit target list. |
| `GetCompShareInstanceUserPrice` | 20 if target args exist | 0 | Reject oversized explicit target list. |
| `DiagnoseBilling` | 0 | 0 | Meta tool; quota controls it. |

`0` means the cap is not applicable for that action, not unlimited by policy accident.

Oversized requests must not be silently truncated.

User-visible messages:

- Target cap: `本次最多支持查询 N 台实例，请缩小范围后重试。`
- History window cap: `历史监控时间窗最多支持 24 小时，请缩短时间范围后重试。`

### 6. Sentinel Error Contract

Add sentinel errors:

```go
var ErrToolCapExceeded = errors.New("tool cap exceeded")
var ErrHistoryWindowExceeded = errors.New("history window exceeded")
```

Layer behavior:

| Layer | Required behavior |
| --- | --- |
| `SafeToolExecutor` cap precheck | Return the sentinel before calling the underlying executor. Do not call API. |
| `executeWithRetry` | Recognize both sentinels as non-retryable, regardless of message text. |
| Main ReAct tool loop | Convert the sentinel into a structured, friendly tool result for the LLM. Do not surface it as a raw `StepError`; the LLM should be able to ask the user to narrow scope or time window. |
| Phase 1 deterministic handler | Return the friendly user message directly; do not fall back to ReAct after a cap precheck failure. |
| Workflow path | Treat sentinel as step failure; stop the workflow and return the friendly message to the user. Do not silently continue. |
| Diagnosis path | Treat sentinel as diagnosis failure; stop and return the friendly message. Do not silently continue. |
| Trace writer | Record a failed / capped tool call with `executed_targets=0` and no raw args/result. |

Rate-limit denial is separate from cap sentinels. It continues to use `governance.ErrRateLimited`.

### 6.1 Rate-Limit Denial Propagation

`governance.ErrRateLimited` has its own propagation contract:

| Layer | Required behavior |
| --- | --- |
| Subject quota precheck | Return `governance.ErrRateLimited` before calling API / LLM. Do not consume retry attempts. |
| `executeWithRetry` | Treat `governance.ErrRateLimited` as non-retryable. |
| Main ReAct tool loop | Convert the denial into a structured, friendly tool result for the LLM. Do not surface raw quota internals. |
| Phase 1 deterministic handler | Return the friendly quota message directly; do not fall back to ReAct. |
| Workflow path | Stop the workflow and return the friendly quota message to the user. Emit a blocked quota step, not a generic API error. |
| Diagnosis path | Stop the diagnosis chain and return the friendly quota message. Emit a blocked quota step, not a generic API error. |
| Init context refresh | Warn to stderr and keep registry trace state observable as `unavailable` / previous snapshot. Do not fail startup solely on quota denial. |
| Trace writer | Record `rate_limit.class=\"read_expensive_tool\"`, the denied action, reason, subject hash, and retry_after_ms. If a tool call trace is emitted for the denied call, set `capped=\"rate_limit\"` and `executed_targets=0`. |

Use the same public-facing quota wording family as existing LLM / mutating quota denials. Do not invent diagnosis-specific quota wording in this ticket.

### 7. Workflow and Diagnosis Internal Calls

Internal calls are not free:

| Path | Subject quota | Turn-scoped budget | Per-call cap |
| --- | --- | --- | --- |
| Main ReAct / direct external | Counts | Counts | Applies |
| `OriginWorkflowInternal` | Counts | Exempt | Applies |
| `OriginDiagnosisInternal` | Counts | Counts | Applies |
| Init context refresh | Counts unless explicitly exempted in test seam | Exempt | Applies |
| Shadow-only planner | LLM quota only | N/A | N/A |

Design rationale:

- Workflow internal steps are part of a confirmed user-level operation. Blocking the third workflow step because of a turn budget is worse than counting it against the subject quota.
- Diagnosis internal steps can loop and should still count toward the turn budget.
- Per-call caps protect the underlying API and must apply regardless of origin.

`DiagnoseBilling` note:

- One `DiagnoseBilling` run may consume several read-expensive quota units because it can call `DescribeCompShareInstance` and price/billing helpers internally.
- This is expected. It is not a quota leak.
- Tests must assert this behavior so future refactors do not "dedupe" it away by accident.

### 8. Turn-Scoped Budget

Add an Engine-owned turn budget for read-expensive calls.

Initial default:

```text
MaxReadExpensiveCallsPerTurn = 20
```

Rules:

- Reset at the start of each `Chat()` user turn.
- Count main ReAct direct external read-expensive tool calls.
- Count `OriginDiagnosisInternal` read-expensive calls.
- Exempt `OriginWorkflowInternal`.
- Exempt planner shadow LLM calls; those are covered by LLM quota.
- On exhaustion, return a friendly cap result and record trace.

This is not a replacement for subject quota or per-call caps.

### 9. Monitor History Guard Coexistence

The new `MaxHistoryWindowSeconds` cap is preventive:

- It rejects over-large monitor history windows before calling API.

The existing `NO_DATA_IN_REQUESTED_WINDOW` guard is reactive:

- It prevents LLM from substituting realtime data when an allowed historical query returns no samples.

Both must coexist. This ticket must not remove the existing monitor no-data guard.

### 10. Trace v0.2 Field Draft

Define trace v0.2 once for this hardening work. Avoid multiple small schema bumps.

Schema version:

```go
const SchemaVersion = "trace.v0.2"
```

Add a runtime block:

```json
"runtime": {
  "planner_mode": "off|shadow",
  "cutover_intents": ["resource", "monitor"]
}
```

Extend `tool_calls[]`:

```json
{
  "capped": "",
  "cap_reason": "",
  "requested_targets": 0,
  "executed_targets": 0,
  "window_seconds": 0
}
```

Allowed `tool_calls[].capped` values:

| Value | Meaning |
| --- | --- |
| `""` | Not capped. |
| `targets` | Rejected because target count exceeds policy. |
| `window` | Rejected because time window exceeds policy. |
| `rate_limit` | Rejected by quota. |

Extend `rate_limit.class` values:

- existing `llm`
- existing `mutating_tool`
- new `read_expensive_tool`

Semantics:

- `requested_targets` is the number requested by LLM / handler when identifiable.
- `executed_targets` is `0` if the call is rejected before API execution.
- If a successful call does not expose a meaningful target count, write `0`.
- `window_seconds` is `0` when no time window applies.
- No raw target IDs or raw timestamps should be added solely for trace.

Migration:

- Existing readers must gracefully handle missing v0.2 fields.
- New tests should include both v0.1 fixture compatibility and v0.2 fixture output.

### 11. Default Cutover-Off Documentation

Update docs / examples so testers do not accidentally use the immature deterministic renderer as the default path.

Rules:

- Manual real-account test commands should unset `USE_INTENT_PLANNER_FOR` by default.
- If planner shadow is desired, use `USE_INTENT_PLANNER=shadow` with no cutover intents.
- `USE_INTENT_PLANNER_FOR=resource,monitor` remains documented as explicit evaluation mode only.

The current Phase 1 handler code remains. Do not delete it.

## Implementation Plan

### Commit 1: Default UX Documentation and Startup Visibility

Files:

- Modify `deploy/conf/agent.yaml.example` if it documents planner cutover defaults.
- Modify CLI / engine startup banner code where config mode is printed.
- Modify relevant docs under `docs/agent/plan/` or setup docs.
- Add tests around mode string derivation if helper code is introduced.

Acceptance:

- Default manual flow is ReAct + optional planner shadow, not deterministic cutover.
- CLI startup banner shows `planner_mode=...` and `cutover_intents=...`.
- No deterministic handler behavior changes.

### Commit 2: Governance Class and Config

Files:

- Modify `internal/governance/ratelimit.go`.
- Modify `internal/config/config.go`.
- Modify `deploy/conf/agent.yaml.example`.
- Modify tests in `internal/governance` and `internal/config`.

Acceptance:

- Add `ClassReadExpensiveTool`.
- Add default read-expensive limits: QPS `3`, daily `500`.
- Add optional config fields `agent.rate_limit.read_expensive_qps` and `agent.rate_limit.read_expensive_daily`.
- Missing config keeps defaults.
- Negative config values fail fast.
- `go test ./internal/governance ./internal/config -count=1` passes.

### Commit 3: Policy Classification and Per-Call Caps

Files:

- Modify `internal/tools/policies.go`.
- Modify `internal/tools/safe_executor.go`.
- Modify `internal/tools/safe_executor_test.go`.

Acceptance:

- `DescribeCompShareInstance` is `read_expensive_default`.
- Price/billing classification uses explicit allowlist, not substring search.
- `GetCompShareInstanceMonitor` has `MaxTargetsPerCall=20` and `MaxHistoryWindowSeconds=86400`.
- `DescribeAvailableCompShareInstanceTypes` and `CheckCompShareResourceCapacity` are classified as `read_expensive_default`.
- Cap precheck returns `ErrToolCapExceeded` / `ErrHistoryWindowExceeded`.
- Cap errors are non-retryable.
- Underlying executor is not called when cap fails.
- Existing reactive monitor no-data guard remains covered by tests.

### Commit 4: Engine Wiring and Internal-Origin Semantics

Files:

- Modify `internal/engine/engine.go`.
- Modify `internal/engine/engine_test.go`.
- Modify workflow / diagnosis tests if they assert step behavior.

Acceptance:

- Read-expensive subject quota is checked for main ReAct, diagnosis internal, workflow internal, and init refresh as specified.
- Workflow internal calls are exempt from turn-scoped budget.
- Diagnosis internal calls count toward turn-scoped budget.
- Per-call caps apply across all origins.
- One `DiagnoseBilling` test proves internal read-expensive calls consume multiple quota units by design.
- Cap sentinel in main ReAct is returned as a friendly tool result, not raw `StepError`.
- Workflow / diagnosis sentinel stops the operation and returns the friendly message.
- Read-expensive quota denial inside workflow / diagnosis stops the chain with a friendly quota message and a blocked quota event.
- Init-context quota denial warns and preserves startup; it must not make `Engine.Init` fail solely due to quota exhaustion.
- `go test ./internal/engine ./internal/tools -count=1` passes.

### Commit 5: Trace v0.2

Files:

- Modify `internal/observability/trace.go`.
- Modify trace recorder tests under `cmd/` and `internal/observability`.
- Modify `scripts/planner_vs_guard_diff.py` only if it assumes trace.v0.1 fields.
- Add `internal/observability/testdata/trace_v0_2_*.json` fixtures.

Acceptance:

- `schema_version` becomes `trace.v0.2`.
- Runtime block writes `planner_mode` and `cutover_intents`.
- Tool call cap fields are present with defaults.
- Read-expensive rate-limit decisions can be represented.
- Existing v0.1 fixtures remain readable by scripts where needed.
- Secret redaction tests still prove raw args/results do not enter trace.

### Commit 6: Real-Account GT Smoke

Files:

- Add sanitized artifact under `eval/capability/`.

Acceptance:

- Run with `deepseek-v4-flash` + ReAct default.
- Include at least:
  - `我现在有哪些机器在跑？`
  - current monitor for one known instance
  - all-running monitor request below 20 targets
  - an over-target monitor request using a controlled test seam or mock if the account has fewer than 21 instances
  - account-level billing hard-block
- Prove real account with 16-18 instances is not accidentally blocked by the new defaults.
- Prove read-expensive quota is consumed and visible in trace.
- Artifact must not include raw trace lines, raw API response, keys, IPs, or full user text if sensitive.

## Review Checklist

- [ ] Default user path remains ReAct unless `USE_INTENT_PLANNER_FOR` is explicitly set.
- [ ] No deterministic renderer promotion.
- [ ] No EntityRegistry hard enforcement.
- [ ] No broad substring action classification.
- [ ] Read-expensive quota applies to workflow / diagnosis internal API calls by the rules above.
- [ ] Cap sentinels do not enter retry.
- [ ] Oversized requests are rejected, not silently truncated.
- [ ] Existing account billing hard-block remains before planner / retrieval / ReAct.
- [ ] Existing monitor no-data reactive guard remains.
- [ ] Trace does not record raw target IDs just to support cap auditing.
- [ ] Multi-replica limiter limitation is documented in code.
- [ ] Real-account smoke uses `deepseek-v4-flash` primary baseline.

## Open Follow-Ups

- Redis / centralized limiter for multi-replica production.
- Hybrid renderer design after anti-abuse guardrails are in place.
- Per-action response field allowlist envelope for LLM summarization.
- More granular `DescribeCompShareInstance` classification if list-all and single-target usage need different quotas.
