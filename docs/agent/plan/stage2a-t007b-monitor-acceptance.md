# Stage 2A T-007b Monitor Acceptance

## Purpose

This document converts the PR #12 monitor stale-reuse probe into explicit T-007b / Phase 1 acceptance criteria.

T-007b itself is still **shadow planner + trace + dashboard**. It must not change the production ReAct path. The fresh-monitor-call requirement becomes a **promotion gate** for the later planner-driven monitor handler, not a hidden behavior change in T-007b.

Reference artifact:

- `eval/capability/2026-05-08-ds-v4-flash-monitor-stale-reuse-probe.md`

## Background

PR #12 showed that `deepseek-v4-flash` auto routing does not reliably refresh adjacent monitor follow-ups after PR #11 disabled object `tool_choice` for this model:

| Probe case | Second-turn monitor call | Result |
| --- | ---: | --- |
| `adjacent_same_metric` | 0 | stale-reuse risk |
| `adjacent_explicit_refresh` | 1 | fresh recalled |
| `adjacent_pronoun_now` | 0 | stale-reuse risk |

The failure mode is concrete: the second turn can return GPU / VRAM monitor values from the previous tool result without calling `GetCompShareInstanceMonitor` again.

## T-007b Shadow Acceptance

T-007b must make this behavior observable in trace without changing runtime behavior.

### Trace Fields

For every user turn, the trace record must include enough information to evaluate monitor freshness:

| Field | Required value / meaning |
| --- | --- |
| `planner.intent` | Planner output intent, e.g. `monitor_query`, `monitor_history`, `mixed_monitor_billing`, `unknown`. |
| `planner.slots.target_refs` | Target refs used by the planner. |
| `planner.slots.metrics` | Requested monitor metrics, e.g. `cpu`, `memory`, `gpu`, `vram`. |
| `planner.slots.time_window` | Monitor time window if present. |
| `tool_calls[].action` | Actual main-path tool calls emitted by the current ReAct engine. |
| `tool_calls[].turn_index` | Current user turn index. |
| `tool_calls[].source` | T-007b values: `main_react`, `workflow_internal`, `diagnosis_internal`, or `shadow_only`. Phase 1 monitor handler adds `planner_handler`. |
| `tool_calls[].args_hash` | Redacted/hash form of tool args for correlation. |
| `renderer.input_tool_call_ids[]` | Tool call ids whose results are passed to the final renderer. Required once the renderer path is instrumented in Phase 1. |
| `renderer.input_tool_args_hashes[]` | Args hashes corresponding to renderer-consumed tool results. Required once the renderer path is instrumented in Phase 1. |
| `freshness.monitor_call_in_current_turn` | Boolean derived by trace writer: true iff this user turn contains `GetCompShareInstanceMonitor`. |

### Shadow Assertions

For each PR #12 case replayed under T-007b shadow mode:

- The planner must classify the second turn as `monitor_query` or `monitor_history`.
- The planner must extract monitor metrics from the second turn when present:
  - `adjacent_same_metric`: at least `gpu` and `vram`.
  - `adjacent_explicit_refresh`: at least `gpu` and `vram`.
  - `adjacent_pronoun_now`: at least `gpu` and `vram`.
- The trace dashboard must report whether the current production path actually called `GetCompShareInstanceMonitor` in the same turn.
- T-007b must not fail the run when the current production path misses the call; it must record this as `monitor_freshness_miss` because T-007b is still shadow-only.

## Phase 1 Monitor Handler Promotion Gate

Before `monitor_query` / `monitor_history` are switched from ReAct + bridge guards to planner-driven handlers, the handler must satisfy the following deterministic rule:

> If `IntentPlan.intent` is `monitor_query` or `monitor_history`, the handler must call `GetCompShareInstanceMonitor` in the current user turn before rendering a user-visible answer. It is forbidden to answer using only monitor tool results already present in conversation history.

### Trace-Level Pass Condition

For every monitor handler test case:

- There is at least one trace event where:
  - `turn_index == current_user_turn`
  - `tool_calls[].action == "GetCompShareInstanceMonitor"`
  - `tool_calls[].source == "planner_handler"`
- `renderer.input_tool_call_ids[]` or `renderer.input_tool_args_hashes[]` references the current-turn monitor result, not only a prior-turn result.
- The final answer may mention previous values for comparison only if the current-turn monitor call succeeded or returned an explicit no-data status.

### PR #12 Regression Fixtures

The following cases from PR #12 must be converted into regression fixtures. Do not copy the raw replies from the artifact; reference the artifact and encode the user turns.

| Fixture id | Turn 1 | Turn 2 | Required assertion |
| --- | --- | --- | --- |
| `monitor_followup_same_metric` | `帮我看下 <target> 这台机器当前的 CPU、内存、GPU 利用率和显存` | `只看刚才那台机器的 GPU 和显存监控` | Turn 2 trace contains current-turn `GetCompShareInstanceMonitor`. |
| `monitor_followup_explicit_refresh` | same as above | `重新查一下刚才那台机器现在的 GPU 利用率和显存，不要复用上一轮` | Turn 2 trace contains current-turn `GetCompShareInstanceMonitor`. |
| `monitor_followup_pronoun_now` | same as above | `它现在 GPU 和显存是多少？` | Turn 2 trace contains current-turn `GetCompShareInstanceMonitor`. |

The fixture target must be resolved through `EntityRegistry`; the test must not hard-code a real account UHostId. Regression fixtures should reuse the same mock registry snapshot pattern as `eval/intent/fixtures.jsonl` monitor cases: `<target>` is a stable fixture alias that resolves to a synthetic instance id/name in the registry snapshot.

## Mixed Scope Boundaries

Monitor freshness must not override higher-priority or mixed-intent routing.

| User message shape | Required planner / handler behavior |
| --- | --- |
| Monitor + account-level billing boundary, e.g. `刚才那台 GPU 怎么样，账号余额还剩多少` | Engine hard-block for account-level billing remains authoritative. The planner may emit a hard-block hint, but no monitor handler should answer the account-level finance portion. |
| Monitor + instance billing, e.g. `刚才那台 GPU 异常吗，扣费是不是也高` | Planner must classify as mixed monitor + instance billing. Handler must gather fresh monitor data for the monitor portion and use instance-billing path for billing facts; renderer must label sources separately. |
| Monitor + SSH / diagnosis, e.g. `刚才那台 GPU 低但 SSH 连不上` | Planner must classify as mixed monitor + diagnosis or diagnosis with monitor evidence. A diagnosis handler may call `DiagnoseSSH`; if the final answer includes current monitor values, it must also call `GetCompShareInstanceMonitor` in the current turn. |
| Monitor + operation, e.g. `刚才那台 GPU 空闲就关机` | Operation confirmation flow remains out of T-007b scope. The later operation handler must not execute mutation based only on prior-turn monitor values; it needs fresh monitor data before presenting a recommendation/confirmation. |
| Non-monitor follow-up, e.g. `4090 显存多大` | Must not force a monitor call solely because the previous turn queried monitor data. |

## Escalation Trigger

No new engine keyword guard or prompt nudge is required solely because PR #12 exists. However, a temporary non-object-tool-choice mitigation may be introduced before Phase 1 if both conditions hold:

- A real user report or high-confidence E2E shows stale monitor data caused or could directly cause a wrong operational decision, such as shutting down a machine that was no longer idle.
- The mitigation is narrow: adjacent monitor follow-up only, no object `tool_choice`, no mutation, and covered by the three PR #12 regression fixtures.

Allowed temporary mitigation:

- Dynamic system/developer nudge telling the model that previous monitor values are stale and it must call monitor again before answering.

Disallowed temporary mitigation:

- New broad keyword guard packs.
- Prompt-only rewrites for unrelated resource, billing, SSH, or operation intents.
- Removing `shouldForceMonitorRecall` before the planner-driven monitor handler passes this document's promotion gate.

## Relationship To `shouldForceMonitorRecall`

`shouldForceMonitorRecall` remains a bridge guard:

- Keep it while production still uses ReAct for monitor intents.
- Keep its capability gate: models with `SupportsObjectToolChoice=false` must not receive object `tool_choice`.
- Delete it only after planner-driven monitor handlers satisfy the Phase 1 promotion gate for all PR #12 regression fixtures and mixed scope boundaries.
