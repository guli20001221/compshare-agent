---
status: draft for review
parent: stage2-intent-planner.md
phase: Phase 1 demo cutover
created: 2026-05-09
primary_baseline: deepseek-v4-flash
---

# Stage 2A Phase 1 Demo Cutover

## Purpose

Phase 0 completed the infrastructure layer: SafeToolExecutor, SecretBoundary,
CapabilityRegistry, EntityRegistry runtime, RateLimiter, Trace v0.1, offline
planner, and shadow planner. Phase 1 starts the strangler migration, but the
first production slice should be deliberately small.

This ticket defines a **demo cutover**:

- opt-in only;
- no default behavior change;
- only valid planner plans can enter deterministic handlers;
- only read-only user-visible query classes are eligible;
- all uncertain cases fall back to the existing ReAct path.

The goal is to demonstrate that Stage 2A can answer common resource and monitor
questions through deterministic handlers without removing the old ReAct path or
the permanent hard-blocks.

## Scope

### Eligible intents

Phase 1 demo cutover may handle only:

| Intent | Demo behavior |
| --- | --- |
| `resource_info` | Resolve target(s), call `DescribeCompShareInstance`, render a concise resource summary. If no target is supplied, list current account instances from `DescribeCompShareInstance`. |
| `monitor_query` | Resolve target(s), call `GetCompShareInstanceMonitor` in the current turn, render current monitor values. Empty `slots.metrics` is allowed and means "render available monitor fields". |

`monitor_history` is **not required** for the first demo slice. It may remain on
the old ReAct path unless the implementation can support its time-window
contract without widening scope.

### Ineligible intents

The demo handler must not cut over:

- `billing_instance`;
- `billing_account_unsupported`;
- `diagnosis`;
- `operation_lifecycle`;
- `expiry_renewal`;
- `mixed_*`;
- `unknown`;
- any mutating action.

The existing account-level billing hard-block remains authoritative. If a user
asks an unsupported account-level billing question, the engine must return the
existing canned console guidance and must not let the planner handler override
that decision.

## Feature Gate

Introduce an explicit opt-in gate:

```text
USE_INTENT_PLANNER_FOR=resource,monitor
```

Rules:

- Empty / unset means current production behavior.
- Unknown values are ignored with a warning, not a panic.
- `resource` enables `resource_info`.
- `monitor` enables `monitor_query`.
- The gate is evaluated per turn and per validated planner result.

This is separate from `USE_INTENT_PLANNER=shadow`. Shadow mode can remain active
for observability, but demo cutover must not depend on shadow mode being enabled.

When `USE_INTENT_PLANNER=shadow` and `USE_INTENT_PLANNER_FOR=...` are both
enabled, the engine must issue **exactly one planner LLM call per user turn**.
That single planner result feeds both:

- the demo cutover decision;
- the `trace.planner` block used by shadow observability.

It must also consume planner LLM quota exactly once. Do not call the planner once
for shadow and again for cutover.

## Cutover Flow

The engine keeps the current hard-block and ReAct flow as the fallback path.

```text
user message
  -> permanent hard-blocks
      -> canned reply if matched
  -> optional planner call, only when USE_INTENT_PLANNER_FOR is non-empty
      -> not cutover-eligible -> old ReAct
      -> eligible intent -> deterministic handler
          -> can handle -> answer
          -> cannot handle before tool call -> old ReAct
          -> tool call attempted and failed -> friendly tool failure reply
```

The handler must not silently call the same external API twice for the same turn.
If it has already attempted a tool call and that call fails, it returns a
friendly failure response instead of falling back to ReAct and risking a second
API call.

### Cutover eligibility predicate

The dispatcher may call a deterministic handler only if all predicates hold:

```go
result.Fallback == false
result.LastValidationCode == ""
result.Plan.HardBlockHint == false
result.Plan.Retrieval.Enabled == false
result.Plan.Confidence >= 0.60
result.Plan.Intent is enabled by USE_INTENT_PLANNER_FOR
result.Plan.Intent is one of {resource_info, monitor_query}
```

`intent.ValidatePlan` remains the schema authority. The predicate above is the
additional runtime cutover gate. Any failed predicate must record a cutover
fallback status and continue through the old ReAct path.

The `0.60` confidence threshold is intentionally low for the demo: it blocks
obvious planner uncertainty while avoiding prompt-tuning work before the first
opt-in slice. Raising this threshold is a post-demo quality decision and must be
based on trace evidence.

Implementation must map the predicate to the actual `intent.PlannerResult` /
`intent.Plan` fields. Do not silently drop any predicate if a field name changes.

`HardBlockHint=true` is observed only; it does not promote to a canned reply in
this demo. The permanent engine hard-block remains the only user-visible
account-level billing boundary.

## Handler Contract

### Common

Handlers must:

- call tools through SafeToolExecutor;
- preserve existing rate-limit and trace hooks;
- emit `StepEvent` records with `source = planner_handler`;
- enforce a per-handler action whitelist before dispatch:
  - resource handler: `DescribeCompShareInstance` only;
  - monitor handler: `GetCompShareInstanceMonitor` only;
- use EntityRegistry snapshots only as observable grounding and target
  resolution support;
- not mutate EntityRegistry except through the normal SafeToolExecutor /
  Engine invalidation hooks already established by T-004b;
- not perform operations, diagnosis, or billing decisions.

Handlers may fall back to ReAct only before they make an external API call.
Out-of-whitelist action construction is a handler bug and must return
fallback-before-tool in tests.

Friendly failure wording after an attempted tool call should be a shared const.
The exact value is the Chinese sentence encoded below to keep this ticket
ASCII-stable:

```go
const FriendlyToolFailureReply = "\u67e5\u8be2\u6682\u65f6\u5931\u8d25\uff0c\u8bf7\u7a0d\u540e\u518d\u8bd5\u3002"
```

The response may include a short non-sensitive action label, but must not include
raw API errors, keys, IPs, balances, or charge amounts.

### Resource info handler

Input:

- `IntentPlan.intent == resource_info`;
- optional `slots.target_refs`.

Behavior:

- If target refs exist, resolve them through EntityRegistry.
- If no target refs exist, call `DescribeCompShareInstance` with the existing
  list semantics (`Limit`/`Offset` default path).
- Render a deterministic Chinese summary using safe fields such as instance id,
  name, state, GPU type/count, CPU, memory, image type, start time, and expire
  time when available.

Fallback:

- unresolved ambiguous target -> old ReAct before tool call;
- validation failure -> old ReAct;
- API failure after tool attempt -> friendly failure reply.

### Monitor query handler

Input:

- `IntentPlan.intent == monitor_query`;
- resolved target refs are required for demo cutover.

Behavior:

- Resolve target refs through EntityRegistry.
- Call `GetCompShareInstanceMonitor` in the current user turn.
- Accept only current-time monitor windows in the demo:
  - `slots.time_window == nil`; or
  - `slots.time_window.type == "preset"` with value `now`, `current`,
    `realtime`, or `today`.
- Any relative or absolute time window, and any preset outside the allow-list,
  must return fallback-before-tool to the old ReAct path.
  `current` and `realtime` are accepted as demo-only aliases for `now`; `today`
  is accepted because users often use it as current-day shorthand. Commit 5 smoke
  must document which values were observed.
- If `slots.metrics` is empty, render all available monitor fields returned by
  the API. This is acceptable for the first demo: answering the user's monitor
  question safely matters more than perfect metric-slot extraction.
- If `slots.metrics` is non-empty, prefer those metrics in the response but keep
  returned values grounded in the current-turn tool result.

Fallback:

- missing target refs -> old ReAct before tool call;
- unresolved or ambiguous target -> old ReAct before tool call;
- non-current `slots.time_window` -> old ReAct before tool call;
- validation failure -> old ReAct;
- API failure after tool attempt -> friendly failure reply;
- `monitor_history` remains old ReAct unless separately enabled by a reviewed
  follow-up.

Promotion rule inherited from `stage2a-t007b-monitor-acceptance.md`:

- the trace for a handled monitor query must include a current-turn
  `GetCompShareInstanceMonitor` call with `source = planner_handler`;
- at least one of `renderer.input_tool_call_ids[]` or
  `renderer.input_tool_args_hashes[]` must reference the current-turn monitor
  result consumed by the handler response;
- the answer must not be rendered solely from previous-turn monitor results.

## Planner Quality Policy

Phase 1 demo does not block on perfect planner quality.

Known Phase 0 smoke observations:

- `monitor_query` can have `slots.metrics = []`;
- account-level billing questions can be hard-blocked by the engine while the
  planner emits `unknown`;
- schema-valid rate was 87.50% in the first real-account smoke.
- The 2026-05-09 Phase 1 smoke observed `deepseek-v4-flash` repeating the
  prompt example confidence value (`0.82`) for both dispatched demo turns.
  Treat the `0.60` threshold as an eligibility guard for the first demo, not as
  calibrated confidence. Post-demo prompt work should diversify or remove
  example confidence values before using confidence as a quality metric.

Demo policy:

- `slots.metrics = []` is acceptable for `monitor_query` if the handler still
  calls monitor and renders available current-turn values.
- `billing_account_unsupported` planner recall is not a demo blocker because the
  permanent engine hard-block owns that boundary.
- invalid planner output must fall back to old ReAct and must not degrade the
  default production path.
- Bare monitor questions without a resolvable target are not a demo blocker.
  They fall back to old ReAct; the handler must not guess "current instance" from
  history unless the planner target ref is validated through provenance.

Prompt/schema tuning can be done after the demo if traces show user-visible
failure modes. Do not widen this ticket solely to improve offline planner scores.

## Trace Requirements

When trace is enabled:

- planner fields must record the actual planner result used for the cutover
  attempt;
- handler tool calls must use `source = planner_handler`;
- resource handler trace must include `DescribeCompShareInstance`;
- monitor handler trace must include current-turn
  `GetCompShareInstanceMonitor`;
- monitor handler trace must populate renderer consumption fields for the
  current-turn monitor result:
  - `renderer.input_tool_call_ids[]`;
  - `renderer.input_tool_args_hashes[]`;
- existing `entity_registry.*`, `rate_limit.*`, and hard-block fields must keep
  their current semantics.

If cutover falls back to ReAct before any handler tool call, trace should make
the fallback observable through an additive planner trace field:

```json
"planner": {
  "cutover_status": "dispatched"
}
```

Allowed `planner.cutover_status` values:

| Value | Meaning |
| --- | --- |
| `""` | Cutover feature disabled or not applicable. |
| `dispatched` | Handler accepted the plan. |
| `fallback_invalid` | Planner result was fallback or invalid. |
| `fallback_low_confidence` | Confidence was below `0.60`. |
| `fallback_hard_block_hint` | Planner emitted `hard_block_hint=true`; engine hard-block remains authoritative. |
| `fallback_ineligible` | Intent is valid but not enabled for demo cutover. |
| `fallback_unresolved_target` | Handler could not resolve a required target before a tool call. |
| `fallback_time_window` | Monitor plan requested a non-current time window. |
| `failure_after_tool` | Handler attempted a tool call, or failed while rendering that tool result, and returned the friendly failure reply. |

The implementation must add tests for these status values before engine cutover
review.

## Non-Goals

- No default cutover.
- No removal of old ReAct path.
- No deletion of `shouldForceMonitorRecall`.
- No new broad keyword guard.
- No operation lifecycle handler.
- No diagnosis handler.
- No instance billing handler.
- No RAG / FAQ integration.
- No prompt-only force-tool fallback.
- No object `tool_choice` for `deepseek-v4-flash`.

## Implementation Plan

### Commit 1: Phase 1 demo handler skeleton

Files:

- `internal/intent/handler.go` or equivalent small package;
- focused unit tests.

Scope:

- define handler request/result types;
- define fallback reason enum;
- implement pure render helpers for resource and monitor summaries;
- no engine wiring.

Acceptance:

- resource summary renderer is deterministic and redacts sensitive fields;
- monitor renderer accepts empty metrics and renders available current-turn
  values;
- handler result can distinguish handled / fallback-before-tool /
  failure-after-tool.
- friendly failure reply is a shared const and does not expose raw API errors;
- action whitelist enforcement exists at handler construction/dispatch time.

### Commit 2: Resource info handler

Scope:

- resolve target refs via EntityRegistry snapshot;
- call `DescribeCompShareInstance` through an injected executor interface;
- render deterministic response;
- no engine wiring.

Acceptance:

- target-by-name and target-by-user-id paths covered;
- no-target list path covered;
- ambiguous target returns fallback-before-tool;
- API failure after attempted tool call returns friendly failure result;
- no mutating action can be emitted;
- out-of-whitelist action dispatch is rejected before tool call.

### Commit 3: Monitor query handler

Scope:

- resolve target refs;
- call `GetCompShareInstanceMonitor` through injected executor;
- support empty `slots.metrics`;
- no engine wiring.

Acceptance:

- valid target emits exactly one monitor tool call;
- missing target returns fallback-before-tool;
- non-current `slots.time_window` returns fallback-before-tool, with a fixture
  where the planner emits `monitor_query` and `time_window=yesterday`;
- empty metrics still renders current-turn values;
- out-of-whitelist action dispatch is rejected before tool call;
- returned trace metadata can prove current-turn monitor call;
- renderer trace metadata references the current-turn monitor result id or args
  hash;
- historical monitor remains out of scope unless explicitly enabled in a
  follow-up review.

### Commit 4: Engine opt-in cutover

Scope:

- add engine-level planner dependency interface;
- add opt-in enabled-intent config;
- preserve permanent hard-block precedence;
- call deterministic handler only for enabled valid plans;
- fall back to old ReAct before handler tool call when handler returns
  fallback-before-tool;
- emit `StepEvent` source `planner_handler`.

Acceptance:

- with gate unset, planner is not called and existing tests pass unchanged;
- with both `USE_INTENT_PLANNER=shadow` and `USE_INTENT_PLANNER_FOR=...`, planner
  LLM is called exactly once per user turn and quota is checked once;
- hard-block happens before planner cutover and remains authoritative;
- invalid / unknown / ineligible plan falls back to old ReAct;
- `HardBlockHint=true` does not produce a canned reply in this demo unless the
  existing engine hard-block matched before planner invocation;
- valid resource plan bypasses ReAct and calls `DescribeCompShareInstance`;
- valid monitor plan bypasses ReAct and calls `GetCompShareInstanceMonitor`;
- handler failure after tool attempt does not perform a second ReAct API call;
- fallback-before-tool then entering ReAct still preserves existing
  `shouldForceMonitorRecall` behavior and its capability gate;
- `planner.cutover_status` is populated for dispatch/fallback/failure cases.

### Commit 5: CLI wiring and smoke

Scope:

- parse `USE_INTENT_PLANNER_FOR`;
- construct the planner used for demo cutover;
- keep shadow mode available;
- add real-account smoke artifact using `deepseek-v4-flash`.

Acceptance:

- `go test ./... -count=1` passes;
- `USE_INTENT_PLANNER_FOR=` default behavior unchanged;
- `USE_INTENT_PLANNER_FOR=resource,monitor` real-account smoke shows at least:
  - one resource-info answer through `planner_handler`;
  - one monitor answer through `planner_handler`;
  - one account-level billing hard-block that does not enter handler;
  - one invalid/unknown planner fallback that uses old ReAct;
- if the smoke includes a mixed-intent fixture, the artifact must report whether
  the planner emitted `mixed_*`; if no mixed fixture is included, document that
  explicitly;
- trace contains no raw secret, IP, password, token, balance, or charge amount;
- `scripts/secret_scan.ps1` passes after the smoke artifact is added;
- smoke artifact does not commit raw trace, raw transcript, or credentials.

## Review Checklist

- Does any code path enable cutover by default?
- Can an invalid planner result change runtime behavior?
- Can `billing_account_unsupported` bypass the permanent hard-block?
- Can the handler execute a mutating action?
- Does a monitor handler answer without a current-turn monitor call?
- Does fallback after a tool failure risk a duplicate API call?
- Is `deepseek-v4-flash` tested without object `tool_choice`?
- Are trace fallback reasons observable?
- Are all code changes covered by tests before implementation?
