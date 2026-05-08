---
status: draft for review
parent: stage2a-phase0-tickets.md
ticket: T-004b
phase: Phase 0
created: 2026-05-08
---

# T-004b EntityRegistry Runtime Contract

## Goal

Turn the T-004a read-only parser/resolver into a thread-safe runtime component that can be refreshed through `SafeToolExecutor`, observed in `trace.v0.1`, and consumed by T-007b shadow planner without changing current ReAct routing behavior.

## Non-Goals

- No business intent cutover.
- No EntityValidator enforcement.
- No ToolArgBuilder enforcement.
- No registry-based blocking inside `SafeToolExecutor`.
- No planner / renderer / dashboard implementation.
- No pagination beyond the existing `Limit=100` single-page fetch. The `Truncated` flag remains observable only.
- No background periodic scheduler. Async refresh is explicit and event-driven only.

## Current Baseline

T-004a already provides:

- `internal/entity/registry.go`
- `internal/entity/snapshot.go`
- `internal/entity/resolve.go`
- `EntityRegistry.Sync(ctx, exec)` using an injected `Executor`
- `ResolveByID`, `ResolveByName`, and `Filter`
- `LastFullSync`, `LastSyncEvent`, `TotalCount`, `Truncated`

T-004b must not assume these fields are safe for concurrent direct access. The current public maps (`Instances`, `NameIndex`) are a T-004a convenience, not a runtime-safe contract.

## Core Decisions

### 1. Phase 0 Boundary: Observable, Non-Binding

T-004b is **observable but non-binding**:

- The registry may be initialized, refreshed, read, snapshotted, and traced.
- The registry may annotate trace fields: `entity_registry.snapshot_id`, `entity_registry.age_seconds`, and `entity_registry.sync_event`.
- The registry must not change tool selection, tool args, LLM routing, or user-facing answers.
- `SafeToolExecutor` must not reject or rewrite a call because the registry says an instance is missing, released, stale, or ambiguous.

Phase 1/T-007b+ will own enforcement. The Stage 2 baseline rule `age <= 30s OR sync_event == "just_refreshed_for_this_turn"` is intentionally **not enforced** in T-004b.

### 2. Ownership and Integration Interface

Choose Engine-owned registry with executor injection.

```go
type Engine struct {
    safeExecutor *tools.SafeToolExecutor
    registry     *entity.EntityRegistry
}

// Registry refresh uses SafeToolExecutor as the executor.
_ = registry.Refresh(ctx, safeExecutor, entity.RefreshReasonInit)
```

This is a deliberate choice over:

- `SafeToolExecutor` holding `*EntityRegistry`
- passing a snapshot into every `ExecuteSafe` call
- storing snapshots in `context.Context`

Reasons:

- Avoids a dependency cycle where the executor both refreshes and validates against the registry.
- Keeps Phase 0 non-binding: the executor can call `DescribeCompShareInstance` safely, but does not consume registry state for policy decisions.
- Keeps T-007b simple: the shadow runner reads immutable snapshots from Engine/registry and writes trace fields, without changing the executor contract.
- Keeps tests mockable: `entity.Executor` remains the narrow seam.
- T-007b accesses registry state through a read-only Engine accessor that returns an immutable snapshot. The exact accessor signature is deferred to the T-007b implementation ticket, but it must not expose mutable maps or the registry lock.

### 3. Snapshot Identity

`snapshot_id` is a deterministic content hash:

```text
sha256:<16 lowercase hex chars>
```

Hash input:

- sorted `InstanceSnapshot` records by `UHostId`
- `TotalCount`
- `Truncated`

Hash input must exclude:

- `LastFullSync`
- `AgeSeconds`
- `SyncEvent`
- transient errors

Rationale: if two refreshes observe the same account state, they should have the same `snapshot_id` even across turns or process restarts. Freshness is represented separately by `age_seconds` and `sync_event`.

`snapshot_id` intentionally uses the first 16 hex chars for trace readability. Unlike `args_hash` / `result_hash`, it correlates one account's instance inventory snapshots rather than arbitrary high-cardinality payloads; collision risk is acceptable at the expected per-account instance scale. If registry scope expands beyond instance inventory, revisit this length before promotion.

### 4. Thread Safety

T-004b must make registry reads/writes safe under concurrent trace writing and refresh calls:

- Add `sync.RWMutex` to `EntityRegistry`.
- Do not return internal map references from public read APIs.
- Add immutable snapshot accessors, for example:

```go
type RegistrySnapshot struct {
    SnapshotID   string
    Instances    map[string]InstanceSnapshot
    NameIndex    map[string][]string
    LastFullSync time.Time
    SyncEvent    SyncEvent
    TotalCount   int
    Truncated    bool
}

func (r *EntityRegistry) Snapshot() RegistrySnapshot
func (r *EntityRegistry) TraceState(now time.Time) RegistryTraceState
```

Implementation may keep package-private maps internally, but external callers must use methods. If the public T-004a fields remain for compatibility, they must be documented as deprecated and must not be used by new runtime code.

### 5. Sync Event State Machine

T-006 currently writes only `""` or `"unavailable"`. T-004b may add these values:

| sync_event | Meaning | snapshot_id | age_seconds | Next request behavior |
|---|---|---:|---:|---|
| `unavailable` | Registry object is absent or no refresh has ever been attempted. | `""` | `0` | Try init refresh when Engine/Chat path next has a refresh opportunity. |
| `init` | First successful blocking refresh during `Engine.Init()`. | current content hash | age since refresh | Reuse snapshot until TTL/invalidation/manual refresh. |
| `sync_refresh` | Successful blocking refresh for TTL stale, manual refresh, or future handler need. | current content hash | age since refresh | Reuse snapshot until next trigger. |
| `warm_cache` | Successful async/background refresh that was not used to answer the current turn. | current content hash | age since refresh | Reuse snapshot. |
| `failed` | A refresh attempt failed. | last successful hash or `""` | last successful age or `0` | Do not suppress future refresh; next blocking trigger retries. |

Failure contract:

- If refresh fails and a previous successful snapshot exists, keep the previous snapshot and set `sync_event="failed"` for observability.
- If refresh fails before any successful snapshot, set `snapshot_id=""`, `age_seconds=0`, `sync_event="failed"`.
- The error must be stored as a non-secret class/message suitable for tests, but raw API params or credentials must not be stored.
- Failure must not block the current ReAct flow in Phase 0. It is trace-only unless the existing tool call itself fails.

T-004b must not introduce a separate `just_refreshed_for_this_turn` value. The Stage 2 baseline uses that phrase as a future enforcement concept; in this runtime contract a current-turn blocking refresh is represented as `sync_refresh` plus the current trace turn metadata. If T-007b or Phase 1 needs a literal `just_refreshed_for_this_turn` event, that requires an explicit trace schema/document update.

### 6. Cold Start Lifecycle

Network calls must not happen in `Engine.New()`.

Cold-start refresh happens in `Engine.Init(ctx)`:

1. `Engine.New()` constructs an empty registry with `sync_event="unavailable"`.
2. `Engine.Init(ctx)` performs a blocking refresh through `SafeToolExecutor`.
3. On success, registry event is `init`.
4. On failure, registry event is `failed`; existing `Init()` behavior must remain compatible with current tests and user flow.

`Engine.InitWithContext(string)` remains a test/helper bypass and must not perform network refresh. Tests that use it either accept `sync_event="unavailable"` or inject registry state explicitly through test-only seams. T-004b must not change this method's public behavior.

Timeout/retry rules:

- Blocking refresh uses the caller-provided `ctx`.
- T-004b must not add a second retry loop around `SafeToolExecutor`; read retry policy remains owned by `ToolExecutionPolicy`.
- Async refresh must use a bounded context derived from the caller request and must not outlive process shutdown indefinitely.

### 7. Refresh and Invalidation Triggers

T-004b must implement trigger bookkeeping even though Phase 0 does not enforce registry freshness.

| Trigger | Required behavior in T-004b |
|---|---|
| Cold start | Blocking refresh in `Engine.Init(ctx)`; event `init` on success. |
| TTL stale | Expose `NeedsRefresh(now)`; default TTL is 30 seconds for API-grounded future handlers, but Phase 0 does not force refresh before normal ReAct calls. |
| Manual refresh_request | Provide a public method for future callers to request blocking refresh; no CLI/user command required in T-004b. |
| Write-tool invalidate | Mark registry invalidated after successful mutating tool/workflow actions listed below. |
| Async warm cache | Provide explicit `WarmRefresh`/equivalent method; no periodic scheduler. |

Write-tool invalidate whitelist:

| Category | Actions / workflows | Invalidate? | Reason |
|---|---|---:|---|
| Create instance | `CreateCompShareInstance`, `CreateInstanceWorkflow` | yes | Instance set changes. |
| Start/stop/reboot | `StartCompShareInstance`, `StopCompShareInstance`, `RebootCompShareInstance`, `StartInstanceWorkflow`, `StopInstanceWorkflow`, `RebootInstanceWorkflow` | yes | Instance state changes. |
| Rename | `ModifyCompShareInstanceName`, `RenameInstanceWorkflow` | yes | Name index changes. |
| Scheduler | `UpdateCompShareStopScheduler`, `DeleteCompShareStopScheduler`, `SetStopSchedulerWorkflow`, `CancelStopSchedulerWorkflow` | yes | Instance metadata/scheduler view changes. |
| Destructive but blocked | `TerminateCompShareInstance` | no runtime success path in current SafeToolExecutor | L2 refused; no successful mutation to observe. |
| Password reset | `ResetCompShareInstancePassword`, `ResetPasswordWorkflow` | no by default | Secret changes, but instance identity/state/name index does not change. |
| Image/team/disk mutations | e.g. `CreateCompShareCustomImage`, `UpdateCompShareTeam`, disk actions | no for T-004b | Not represented in `InstanceSnapshot`; future registries may add domains. |

Invalidate means: set an internal invalidated flag/reason. It does **not** mean immediate blocking refresh, and it does **not** mean blocking later tool calls.

Workflow invalidation happens after the workflow returns a successful final result, not after each intermediate read step. Direct external action invalidation happens after `SafeToolExecutor` returns success for that action.

Hook location is Engine-owned, not SafeToolExecutor-owned. Direct external actions call `e.registry.MarkInvalidated(action)` in the existing post-success `executeSafeTool` / `executeTool` path. Workflow actions call it after `executeWorkflow` returns a successful workflow result. `SafeToolExecutor` must remain unaware of registry state.

### 8. Trace Contract

T-004b populates the `entity_registry` block reserved by T-006:

```json
"entity_registry": {
  "snapshot_id": "sha256:0123456789abcdef",
  "age_seconds": 12,
  "sync_event": "init"
}
```

Rules:

- If registry exists and has state, write the current `snapshot_id`, `age_seconds`, and `sync_event`.
- If registry is absent, write `snapshot_id=""`, `age_seconds=0`, `sync_event="unavailable"`.
- If last refresh failed, write `sync_event="failed"` and preserve the previous successful `snapshot_id` when one exists.
- Trace writing must not acquire locks for long-running I/O. It should copy registry state under read lock and release before writing the trace file.

## Implementation Plan

### Commit 1: Thread-Safe Registry State

Files:

- Modify: `internal/entity/registry.go`
- Modify: `internal/entity/resolve.go`
- Modify: `internal/entity/registry_test.go`
- Modify as needed: `internal/entity/registry_integration_test.go`

Scope:

- Add mutex protection.
- Add immutable `Snapshot()` / trace-state accessors.
- Ensure maps returned from snapshots are deep copies.
- Keep existing resolver behavior unchanged.
- Do not touch engine, tools, trace writer, or CLI.

Acceptance:

- `go test ./internal/entity -count=1 -race` passes.
- Existing resolve/filter tests still pass.
- New test proves mutating a returned snapshot map does not alter the registry.
- New test proves `ResolveByID` / `ResolveByName` can run concurrently with `SyncFromDescribe` without a race.

### Commit 2: Refresh Lifecycle and Sync Events

Files:

- Modify: `internal/entity/registry.go`
- Modify: `internal/entity/registry_test.go`

Scope:

- Introduce typed `SyncEvent` / `RefreshReason`.
- Implement `Refresh(ctx, exec, reason)` or equivalent wrapper around current `Sync`.
- Implement `NeedsRefresh(now)` and invalidation bookkeeping.
- Implement `MarkInvalidated(action string)` based on the whitelist in this ticket.
- Implement failed-refresh behavior without clearing the last successful snapshot.
- No engine wiring yet.

Acceptance:

- Unit test: initial state traces as `unavailable`.
- Unit test: first successful refresh records `init`.
- Unit test: TTL/manual refresh records `sync_refresh`.
- Unit test: async warm refresh records `warm_cache`.
- Unit test: failed refresh with previous snapshot preserves `snapshot_id` and records `failed`.
- Unit test: failed refresh with no previous snapshot returns empty `snapshot_id` and records `failed`.
- Unit test: each invalidation whitelist entry marks invalidated; non-whitelist actions do not.

### Commit 3: Engine and SafeToolExecutor Integration

Files:

- Modify: `internal/engine/engine.go`
- Modify: `internal/engine/engine_test.go`
- Modify as needed: `cmd/agent.go`

Scope:

- Engine owns `*entity.EntityRegistry`.
- `Engine.New()` constructs an empty registry without network I/O.
- `Engine.Init(ctx)` refreshes registry via `SafeToolExecutor`.
- Successful mutating tool/workflow calls invoke registry invalidation bookkeeping.
- No registry-based blocking or arg rewriting.
- No planner or handler integration.

Acceptance:

- Unit/integration test proves registry refresh path uses `SafeToolExecutor` by injecting a spy executor/policy hook.
- Unit test proves `Engine.New()` does not call `DescribeCompShareInstance`.
- Unit test proves `Engine.Init()` calls `DescribeCompShareInstance` through the safe path.
- Unit test proves a successful `StartCompShareInstance` or `StartInstanceWorkflow` marks registry invalidated but does not block the user flow.
- `go test ./internal/engine ./internal/entity ./internal/tools -count=1` passes.

### Commit 4: Trace Population

Files:

- Modify: `cmd/trace.go` or current trace recorder file
- Modify: `cmd/trace_test.go`
- Modify as needed: `internal/engine/engine.go`

Scope:

- Add a registry state supplier to the trace recorder or engine trace finish path.
- Populate `entity_registry.snapshot_id`, `age_seconds`, and `sync_event`.
- Preserve T-006 behavior when registry is absent.

Acceptance:

- Unit test: trace line contains `sync_event="unavailable"` when registry supplier is nil.
- Unit test: trace line contains `init` and non-empty `snapshot_id` after successful init refresh.
- Unit test: trace line contains `failed` after refresh failure and does not leak raw API errors containing secrets.
- Unit test: trace writer reads registry via copied snapshot, not by retaining map pointers.
- `go test ./cmd ./internal/observability ./internal/entity -count=1` passes.

### Commit 5: Real-Account Smoke Artifact

Files:

- Add: `eval/capability/2026-05-08-t004b-entity-registry-runtime-smoke.md`

Scope:

- Run one real-account session with `COMPSHARE_TRACE_ENABLED=1`.
- Use deepseek-v4-flash as the primary Stage 2A baseline unless explicitly unavailable.
- Do not commit raw trace JSONL, raw transcript, `agent.yaml`, or `.env.local`.

Acceptance:

- Artifact records:
  - model/base URL
  - command shape
  - number of trace lines
  - observed `entity_registry.sync_event` values
  - whether `snapshot_id` is stable across unchanged turns
  - whether `age_seconds` increases across turns
  - secret scan result for the artifact
- Artifact must not contain real UHostIds, public/private keys, IPs, passwords, or user prompt text.

## Review Checklist

- [ ] Public maps are not used as the runtime read contract.
- [ ] Registry refresh goes through `SafeToolExecutor`; there is no direct `external.Execute` call in engine/runtime paths.
- [ ] Registry state is observable in trace but does not affect routing, args, or blocking.
- [ ] `failed` state preserves previous successful snapshot when available.
- [ ] Snapshot hash is deterministic and excludes freshness fields.
- [ ] Invalidation whitelist is explicit; no `strings.Contains("Instance")` style broad matching.
- [ ] Async refresh is explicit and bounded; no hidden periodic goroutine.
- [ ] Tests include `-race` for `internal/entity`.
- [ ] Real-account artifact contains only aggregate/sanitized data.

## Follow-Up After T-004b

- T-007b consumes registry snapshots for shadow slot validation and trace fields.
- Phase 1 handlers enforce freshness and entity validation.
- Pagination for accounts with more than 100 instances must be implemented before any production cutover that relies on full account inventory.
- A future registry package may split resource, billing, and knowledge entities into separate domains; T-004b remains instance-only.
