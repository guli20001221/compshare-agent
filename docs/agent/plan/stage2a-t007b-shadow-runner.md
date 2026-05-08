---
status: draft for review
parent: stage2a-phase0-tickets.md
ticket: T-007b
phase: Phase 0
created: 2026-05-09
---

# T-007b Shadow Runner Contract

## Goal

Run the T-007a IntentPlan planner in production shadow mode, write its output to `trace.v0.1`, and generate planner-vs-runtime reports without changing the current ReAct user path.

T-007b is an observability ticket. It must make planner behavior measurable before Phase 1 handler promotion, especially the monitor freshness cases defined in `docs/agent/plan/stage2a-t007b-monitor-acceptance.md`.

## Non-Goals

- No business intent cutover.
- No planner-driven handler execution.
- No tool argument rewrite from planner slots.
- No `tool_choice` changes.
- No new keyword guard, prompt nudge, or bridge guard.
- No dashboard web UI; markdown reports are enough.
- No renderer instrumentation beyond preserving the reserved `renderer.input_tool_*` fields already defined by trace.v0.1.
- No raw user text, raw entity IDs, raw tool args, or raw tool results in trace artifacts.

## Dependencies

Already satisfied:

- T-001 SafeToolExecutor.
- T-004b EntityRegistry runtime, including immutable trace state.
- T-006 trace writer and CLI/engine wiring.
- T-007a IntentPlan types / validator / offline planner.

Open dependency:

- T-005 RateLimiter / QuotaManager.

T-007b implementation that performs live planner LLM calls must not be enabled in real-account runs until T-005 is merged or the call path has an equivalent explicit quota hook. Unit tests and offline smoke may use mock planner clients before T-005.

## Core Decisions

### 1. Shadow-Only Boundary

Shadow mode is controlled by an environment variable:

```text
USE_INTENT_PLANNER=shadow
```

When unset or set to any other value:

- The planner must not issue an LLM call.
- `trace.planner.enabled` remains `false`.
- User-visible behavior must remain byte-for-byte equivalent except for trace metadata defaults.

When set to `shadow`:

- The planner may run after receiving the user turn and before trace finalization.
- Planner output may populate `trace.planner`.
- Planner output must not change LLM messages, tool choice, tool args, tool execution order, hard-block decisions, final rendering, or user-visible replies.
- Planner failure must not fail the user request. It records a fallback/invalid trace state and emits at most a stderr warning.

### 2. Trace Privacy Projection

Do not write raw `intent.Plan` directly to trace.

`trace.planner.slots` must be populated through a projection layer:

```json
{
  "target_refs": [
    {
      "type": "name",
      "source": "user_text",
      "value_hash": "sha256:...",
      "source_span_hash": "sha256:..."
    }
  ],
  "metrics": ["gpu", "vram"],
  "time_window": {
    "type": "relative",
    "value_hash": "sha256:..."
  }
}
```

Rules:

- `metrics` are enum values and may be written plainly.
- `target_refs[].value`, `target_refs[].source_span`, and user-provided IDs must be hashed before trace.
- `time_window.value` must be hashed unless it is one of a small canonical enum set such as `now`, `today`, `yesterday`, or `last_1h`.
- The trace projection may include `type` and `source` plainly.
- The projection must be deterministic for equal plans.
- The projection must not include raw entity names, UHostIds, user text spans, IP addresses, keys, or billing values.

### 3. Planner Trace Semantics

Populate `observability.PlannerTrace` as follows:

| Field | Disabled | Shadow success | Shadow fallback / invalid |
| --- | --- | --- | --- |
| `enabled` | `false` | `true` | `true` |
| `model` | `""` | configured LLM model | configured LLM model |
| `latency_ms` | `0` | measured planner latency | measured planner latency |
| `schema_valid` | `false` | `true` | `false` |
| `intent` | `""` | planner intent | `unknown` |
| `slots` | empty arrays / null time window | sanitized projection | empty or sanitized fallback projection |
| `confidence` | `0` | planner confidence | `0` |
| `hard_block_hint` | `false` | planner hint | `false` unless fallback plan explicitly sets it |

Token counts remain `0` until the LLM client exposes usage data.

### 4. Engine Hard-Block Observability

`trace.engine_hard_block` is currently reserved but not populated for account-level canned replies. T-007b needs this field for planner-vs-runtime comparison.

Add an observability-only signal for `isAccountBillingUnsupported`:

```json
{
  "engine_hard_block": {
    "hit": true,
    "category": "account_billing_unsupported"
  }
}
```

Constraints:

- Do not change the canned reply.
- Do not print an extra CLI step.
- Do not expose raw user text.
- Do not make planner output participate in the hard-block decision.
- When no hard-block occurs, write `hit=false` and `category=""`.

### 5. Registry Snapshot Input

The shadow planner receives the current `*entity.EntityRegistry` or an immutable snapshot via an engine-owned read-only accessor.

Rules:

- Do not expose mutable maps or registry locks outside engine/entity packages.
- Do not refresh the registry just for the planner.
- Do not block user flow if the registry is unavailable or stale.
- The trace line already records `entity_registry.snapshot_id`, `age_seconds`, and `sync_event`; planner trace does not duplicate those fields.

### 6. Monitor Freshness Reporting

T-007b must implement the shadow side of `stage2a-t007b-monitor-acceptance.md`:

- The three PR #12 follow-up cases must be replayable in shadow mode.
- The planner must classify the second turn as `monitor_query` or `monitor_history`.
- The report must mark `monitor_freshness_miss` when planner intent is monitor but the same trace record lacks `GetCompShareInstanceMonitor`.
- A miss is not a runtime failure in T-007b; it is evidence for Phase 1 handler promotion.

Phase 1 handler pass conditions using `source="planner_handler"` and `renderer.input_tool_*` remain promotion gates only. T-007b must define and report them, not implement the handler.

## Implementation Plan

### Commit 1: Planner Trace Projection

Files:

- Create: `internal/intent/trace_projection.go`
- Test: `internal/intent/trace_projection_test.go`
- Modify as needed: `internal/observability/trace.go`

Scope:

- Convert `intent.PlannerResult` into `observability.PlannerTrace`.
- Hash sensitive / user-provided slot values as defined in "Trace Privacy Projection".
- Preserve empty `target_refs` / `metrics` arrays and nullable `time_window`.
- Do not call LLM or touch engine.

Acceptance:

- Unit test: disabled planner projection writes `enabled=false` and empty slots.
- Unit test: valid monitor plan writes `intent=monitor_query`, metric enums, confidence, and `schema_valid=true`.
- Unit test: target ref value and source span are hashed; raw UHostId / raw name / raw source span do not appear in marshaled trace JSON.
- Unit test: equal plans produce stable projection hashes.
- `go test ./internal/intent ./internal/observability -count=1` passes.

### Commit 2: Shadow Runner Skeleton

Files:

- Create: `internal/intent/shadow.go`
- Test: `internal/intent/shadow_test.go`

Scope:

- Add a `ShadowRunner` that wraps the existing `Planner`.
- Accept input: user text, prior text, registry reference/snapshot, model/base URL metadata.
- Return a projected `observability.PlannerTrace`.
- Handle planner LLM errors and validation fallback without returning an error to production flow.
- Do not wire engine/CLI yet.

Acceptance:

- Unit test: disabled mode does not call planner LLM.
- Unit test: shadow mode calls planner once and returns `enabled=true`.
- Unit test: planner error returns `enabled=true`, `schema_valid=false`, `intent=unknown`, and no propagated error.
- Unit test: fallback result returns `schema_valid=false`.
- `go test ./internal/intent -count=1` passes.

### Commit 3: Engine / CLI Shadow Wiring

Files:

- Modify: `cmd/agent.go`
- Modify: `cmd/trace.go`
- Modify: `internal/engine/engine.go`
- Test: `cmd/trace_test.go`
- Test: `internal/engine/engine_test.go`

Scope:

- Add env parsing for `USE_INTENT_PLANNER=shadow`.
- Wire a planner trace supplier into `cliTraceRecorder`, analogous to the registry trace supplier.
- Populate `trace.planner` before trace `Finish`.
- Populate `trace.engine_hard_block` for account-level canned replies.
- Preserve production behavior when shadow mode is disabled.

Rate-limit boundary:

- If T-005 is not merged, live shadow mode must remain disabled for real-account runs. Tests may use a mock planner.
- Once T-005 exists, planner LLM calls must pass through its quota hook before enabling real-account shadow smoke.

Acceptance:

- Unit test: `USE_INTENT_PLANNER` unset -> no planner call and `planner.enabled=false`.
- Unit test: `USE_INTENT_PLANNER=shadow` -> planner trace appears in JSONL but reply and tool calls are unchanged.
- Unit test: account billing hard-block trace writes `engine_hard_block.hit=true` and category without adding a CLI step.
- Unit test: planner failure still writes a trace line and does not change user reply/error.
- Grep check: planner output is not used to set `ToolChoice`, tool args, or canned replies.
- `go test ./cmd ./internal/engine ./internal/intent ./internal/observability -count=1` passes.

### Commit 4: Shadow Regression Fixtures

Files:

- Create: `eval/intent/shadow_monitor_fixtures.jsonl`
- Create: `eval/intent/shadow_monitor_eval_test.go`
- Modify as needed: `eval/intent/fixtures.go` or local test helpers

Scope:

- Encode the three PR #12 monitor follow-up cases by reference, without copying raw model replies.
- Add mixed-boundary cases from `stage2a-t007b-monitor-acceptance.md`.
- Use mock registry snapshots; do not hard-code real UHostIds.
- Assert trace-level conditions, not user-visible answer text.

Acceptance:

- Unit/eval test: all three PR #12 cases produce planner intent `monitor_query` or `monitor_history` on turn 2.
- Unit/eval test: the report marks `monitor_freshness_miss` when current-turn production trace lacks monitor tool call.
- Unit/eval test: account-level billing mixed case preserves hard-block authority.
- Unit/eval test: non-monitor follow-up does not require monitor freshness.
- `go test ./eval/intent -count=1` passes.

### Commit 5: Planner-vs-Runtime Dashboard

Files:

- Create: `scripts/planner_vs_guard_diff.py`
- Create: `scripts/testdata/planner_vs_guard_trace.jsonl`
- Test: script self-test or `go test` wrapper if preferred by local convention

Scope:

- Read `agent-trace-*.jsonl`.
- Aggregate planner intent counts, schema-valid rate, fallback/invalid count, engine hard-block count, monitor freshness misses, and mixed-boundary outcomes.
- Generate a markdown report.
- Do not require raw transcripts or raw tool payloads.

Acceptance:

- Script produces markdown from testdata.
- Report includes total turns, planner-enabled turns, intent distribution, schema-valid rate, account hard-block agreement, and monitor freshness miss count.
- Report flags `monitor_freshness_miss` for trace records where planner intent is monitor and no current-turn monitor tool call exists.
- Script handles missing planner block / disabled planner records without crashing.

### Commit 6: Real-Account Shadow Smoke Artifact

Files:

- Add: `eval/capability/2026-05-09-t007b-shadow-runner-smoke.md`

Scope:

- Run one real-account shadow smoke with `deepseek-v4-flash`, `COMPSHARE_TRACE_ENABLED=1`, and `USE_INTENT_PLANNER=shadow`.
- Requires T-005 or an equivalent quota hook before running live planner LLM calls.
- Do not commit raw trace JSONL, raw transcript, temp config, or `.env.local`.

Acceptance:

- Artifact records model/base URL, command shape, trace line count, planner-enabled count, schema-valid rate, observed intents, registry trace fields, and dashboard summary path/output excerpt.
- Artifact records whether the three PR #12-style monitor follow-up cases are represented in shadow traces.
- Artifact secret scan passes.
- Artifact contains no real UHostIds, keys, IPs, passwords, raw user prompts, raw tool args, or raw tool results.

## Review Checklist

- [ ] Shadow mode disabled means no planner LLM calls.
- [ ] Shadow mode never changes current ReAct routing, tool choice, tool args, or final reply.
- [ ] Live planner calls are not enabled before T-005 or equivalent quota hook.
- [ ] Planner trace slots are sanitized projections, not raw `intent.Plan`.
- [ ] Account hard-block observability is populated without changing user-visible behavior.
- [ ] Registry is read through immutable accessors only.
- [ ] PR #12 monitor follow-up cases are covered as trace assertions.
- [ ] Dashboard consumes trace only; no raw transcripts or payloads.
- [ ] `deepseek-v4-flash` is used for real-account smoke unless one of the documented inability reasons applies.

## Follow-Up After T-007b

- Phase 1 planner-driven handlers decide whether to promote monitor/resource/billing paths.
- `shouldForceMonitorRecall` can be deleted only after the monitor handler passes `stage2a-t007b-monitor-acceptance.md`.
- Renderer `input_tool_call_ids` / `input_tool_args_hashes` are populated when the Phase 1 renderer path is instrumented.
