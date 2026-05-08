# Stage 2A T-006 Trace / Audit Skeleton

## Purpose

T-006 provides the trace writer that later Stage 2A work depends on. It must make the current ReAct path observable without changing routing, planner, tool, or renderer behavior.

This ticket is infrastructure only. It does not turn on IntentPlan shadow mode, does not implement a dashboard, and does not promote any planner handler.

Reference contracts:

- `docs/agent/plan/stage2-intent-planner.md` section 3.10
- `docs/agent/plan/stage2a-t007b-monitor-acceptance.md`
- `internal/security/secret_boundary.go::RedactForTrace`

## Dependencies

- T-001 SafeToolExecutor is merged and provides `SafeToolResult.TraceResult`, sanitized args, retry attempts, and engine step hooks.
- T-002 SecretBoundary is merged and provides `RedactForTrace(any) any`.
- PR #13 is merged and defines the monitor freshness trace fields that T-006 must reserve.

T-004b and T-005 are not required to implement T-006, but T-007b must wait for all three: T-004b, T-005, and T-006.

## Non-Goals

- No IntentPlan shadow runner. `planner.enabled=false` or `planner=null` is acceptable until T-007b.
- No planner-vs-guard dashboard. That belongs to T-007b.
- No EntityRegistry refresh implementation. T-006 may record an empty or unavailable registry block.
- No RateLimiter implementation. T-006 may reserve fields for future rate-limit audit, but it must not enforce quotas.
- No behavior change in `engine.Chat`.
- No raw prompt, raw user message, raw API key, raw token, raw password, raw billing amount, or raw IP address in trace.

## Runtime Switches

Trace writing must be explicit and safe by default:

| Env var | Meaning |
| --- | --- |
| `COMPSHARE_TRACE_ENABLED=1` | Enables JSONL trace writing. When unset, the trace writer is a no-op. |
| `COMPSHARE_TRACE_DIR=<path>` | Optional trace directory. Default: `logs`. Tests must use a temp dir. |

The CLI should still run normally when tracing is disabled or when the trace writer fails to initialize. If writing a trace line fails after initialization, emit a local warning to stderr but do not fail the user request.

## Trace Schema V0.1

Each user turn writes exactly one JSON object when tracing is enabled. The object is append-only and versioned.

```json
{
  "schema_version": "trace.v0.1",
  "trace_id": "uuid-or-random-id",
  "turn_id": "uuid-or-random-id",
  "turn_index": 1,
  "timestamp": "2026-05-08T12:00:00+08:00",
  "user_msg_hash": "sha256(redacted user msg)",
  "planner": {
    "enabled": false,
    "model": "",
    "latency_ms": 0,
    "input_tokens": 0,
    "output_tokens": 0,
    "schema_valid": false,
    "intent": "",
    "confidence": 0,
    "hard_block_hint": false
  },
  "engine_hard_block": {
    "hit": false,
    "category": ""
  },
  "entity_registry": {
    "snapshot_id": "",
    "age_seconds": 0,
    "sync_event": ""
  },
  "tool_calls": [
    {
      "id": "tool-call-id-if-known",
      "turn_index": 1,
      "action": "GetCompShareInstanceMonitor",
      "source": "main_react",
      "args_hash": "sha256(redacted canonical args)",
      "latency_ms": 123,
      "attempts": 1,
      "status": "success",
      "error_class": "",
      "result_hash": "sha256(redacted canonical result)"
    }
  ],
  "renderer": {
    "model": "",
    "latency_ms": 0,
    "attribution_mode": "",
    "input_tool_call_ids": [],
    "input_tool_args_hashes": []
  },
  "freshness": {
    "monitor_call_in_current_turn": true
  },
  "retrieval": {
    "enabled": false,
    "kb_version": "",
    "hits": 0
  },
  "outcome": {
    "total_latency_ms": 456,
    "total_tokens": 0,
    "attempted_hallucinated_count": 0,
    "escaped_hallucinated_count": 0,
    "kb_conflict_count": 0
  }
}
```

### Field Rules

- `user_msg_hash` must be a hash, not raw user text.
- `tool_calls[].args_hash` must be computed from canonical JSON after `RedactForTrace`.
- `tool_calls[].result_hash` must be computed from canonical JSON after `RedactForTrace`.
- `tool_calls[].source` values for T-006: `main_react`, `workflow_internal`, `diagnosis_internal`, `knowledge_local`, `init_context`.
- T-007b may add `shadow_only`; Phase 1 monitor handlers may add `planner_handler`.
- `freshness.monitor_call_in_current_turn` is derived from `tool_calls[].action == "GetCompShareInstanceMonitor"` for the current `turn_index`.
- `renderer.input_tool_call_ids` and `renderer.input_tool_args_hashes` must exist in the schema. T-006 may write empty arrays because the current ReAct path has no separate renderer instrumentation. Phase 1 monitor handler promotion must populate them before using the PR #13 renderer-consumption assertion.
- Unknown future fields must be additive. Do not rename V0.1 fields without introducing a new `schema_version`.

## Implementation Plan

### Commit 1: Trace Types And Writer

Files:

- Create `internal/observability/trace.go`
- Create `internal/observability/trace_test.go`

Scope:

- Define `TraceRecord`, `PlannerTrace`, `ToolCallTrace`, `RendererTrace`, `FreshnessTrace`, and related structs.
- Implement `Writer` with `Append(record TraceRecord) error`.
- Implement daily JSONL path: `agent-trace-YYYY-MM-DD.jsonl`.
- Implement file creation with `0600` or stricter. Do not block Windows if POSIX mode semantics are unavailable.
- Implement deterministic hashing helper over canonical JSON.
- Apply `security.RedactForTrace` before hashing any args/result payload.

Tests:

- Writer appends one valid JSON object per line.
- Two appends produce two lines, not overwritten output.
- Args/result hashes are stable across map iteration order.
- A fixture containing API keys, Bearer tokens, password, billing amount, and IP address produces no raw secret in the JSONL line.

### Commit 2: Engine/CLI Trace Wiring

Files:

- Modify `cmd/agent.go`
- Modify `internal/engine/engine.go` only if StepEvent lacks the minimum fields needed by the trace adapter.
- Add tests in the narrowest package that owns the new behavior.

Scope:

- Build a per-turn recorder in the CLI when `COMPSHARE_TRACE_ENABLED=1`.
- Wrap the existing `onStep` callback so CLI output remains unchanged while trace events are collected.
- After `Engine.Chat` returns, write one `TraceRecord`.
- Preserve current behavior when tracing is disabled.
- Preserve current behavior when trace write fails, except for a stderr warning.

Minimum data to capture:

- current turn index
- hard-block hit/category when available
- tool call action/source/args hash
- tool result status/error class/result hash when available
- total latency
- `freshness.monitor_call_in_current_turn`

If `StepEvent` must be extended, add optional fields only. Existing tests must not require changes except where they explicitly assert the new trace behavior.

### Commit 3: Retention And Cleanup

Files:

- Modify `internal/observability/trace.go`
- Extend `internal/observability/trace_test.go`

Scope:

- Implement `Cleanup(dir string, retentionDays int, now time.Time) error`.
- Delete only files matching `agent-trace-YYYY-MM-DD.jsonl`.
- Default retention: 30 days.
- Never recursively delete directories.

Tests:

- Deletes trace files older than 30 days.
- Keeps files at exactly 30 days or newer.
- Ignores non-trace files and non-matching paths.

### Commit 4: Real-Account Smoke Documentation

Files:

- Update this ticket or add a short artifact under `eval/capability/` after running the smoke.

Scope:

- Run one 10-step E2E with `COMPSHARE_TRACE_ENABLED=1`.
- Confirm the trace file has one line per user turn.
- Confirm no raw `COMPSHARE_PUBLIC_KEY`, `COMPSHARE_PRIVATE_KEY`, `LLM_API_KEY`, Jupyter token, password, or Bearer token appears in the trace.
- Confirm at least one monitor turn has `freshness.monitor_call_in_current_turn=true`.

Use `deepseek-v4-flash` as the primary baseline unless the run cannot be verified for one of the allowed reasons listed in `stage2a-phase0-tickets.md`.

## Acceptance

- [ ] `internal/observability/trace.go` exists and exposes a small writer API; no package import cycle.
- [ ] JSONL output is versioned with `schema_version="trace.v0.1"`.
- [ ] Tracing is disabled by default and enabled by `COMPSHARE_TRACE_ENABLED=1`.
- [ ] Every enabled `Engine.Chat` turn writes exactly one trace line through the CLI path.
- [ ] Trace lines contain no raw secrets and use `RedactForTrace` before hashing args/results.
- [ ] `tool_calls[].source` distinguishes at least `main_react`, `workflow_internal`, `diagnosis_internal`, `knowledge_local`, and `init_context`.
- [ ] `freshness.monitor_call_in_current_turn` is computed from current-turn tool calls.
- [ ] `renderer.input_tool_call_ids[]` and `renderer.input_tool_args_hashes[]` exist as schema fields, even if empty in T-006.
- [ ] 30-day cleanup is unit-tested with a fake clock / injected `now`.
- [ ] `go test ./... -count=1` passes.
- [ ] `scripts/secret_scan.ps1` passes.
- [ ] With tracing enabled, a real-account 10-step E2E writes the expected number of trace lines and contains no raw secrets.

## Review Checklist

- No raw user message in trace.
- No raw tool args or tool result in trace.
- No trace write failure can fail a user chat.
- No T-007b planner code in this PR.
- No RateLimiter code in this PR.
- No EntityRegistry refresh code in this PR.
- No broad keyword guard or routing behavior change in this PR.

## Follow-Up To T-007b

T-007b should consume this schema by adding:

- `planner.enabled=true`
- planner model/latency/tokens/schema validity
- planner intent/confidence/hard-block hint
- `tool_calls[].source="shadow_only"` for shadow-only planner observations if needed
- dashboard aggregation over planner-vs-guard and monitor freshness misses

Phase 1 monitor handler promotion should later populate:

- `tool_calls[].source="planner_handler"`
- `renderer.input_tool_call_ids[]`
- `renderer.input_tool_args_hashes[]`
