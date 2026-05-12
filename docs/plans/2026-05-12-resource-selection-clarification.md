# Resource Selection Clarification Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add a first-class resource selection step so the agent asks the user to choose an instance instead of silently using the first or most recent instance when a single target is required.

**Architecture:** Keep API-grounded answering unchanged. Add a narrow engine-level pending selection state that is created only when Phase 1 cutover needs one concrete instance but the planner did not provide a unique target. The selection reply is deterministic; after the user selects a candidate, the engine resumes the original intent with the selected instance ID.

**Tech Stack:** Go, existing `internal/engine`, `internal/intent`, `internal/entity`, existing planner cutover and grounded renderer paths.

---

## Scope

This slice covers only instance selection / clarification for the current Phase 1 demo path.

In scope:

- `monitor_query` with no target, ambiguous target, or unresolved human name when a concrete instance is required.
- Next-turn continuation by ordinal (`1`, `2`, `第2个`, `选第二台`), exact UHostId, or exact candidate name.
- Deterministic candidate list rendering using safe instance fields only: ID, name, state, GPU type, GPU count, CPU, memory, zone, charge type.
- Direct continuation after a valid selection by re-running the original handler with the selected UHostId.
- Single-candidate case may continue directly without asking.

Out of scope:

- IP reverse lookup such as `117.50.121.35 无法连接`.
- SSH login, remote command execution, or reading/changing files inside an instance.
- Lifecycle operations such as stop/start/reboot/reset password.
- Historical monitor windows.
- Full-account anomaly scan such as "which machines are idle" as an automatic monitoring sweep.
- Diagnosis workflow selection beyond preserving the boundary for a future slice.
- RAG / FAQ integration.

## User-Facing Behavior

When the user asks a question that requires one instance but does not identify one, the agent should return a short deterministic clarification:

```text
我需要先确认你要看哪台实例。请选择一个：

1. qa-shadow-20260417-4090 (uhost-...) — Running, GPU=4090 x1, CPU=16, 内存=65536 MB, cn-wlcb-01
2. host (uhost-...) — Running, GPU=4090 x1, CPU=16, 内存=65536 MB, cn-wlcb-01

你可以回复序号、实例 ID 或实例名称。
```

The next user turn can be `1`, `选第二台`, `uhost-...`, or an exact candidate name. The engine should then continue the original intent, not start a new unrelated conversation.

The trace must make this state explicit. A turn that asks the user to select a resource is not a failed monitor turn; it is a successful clarification turn. It must write planner `cutover_status="selection_required"` so offline checks can distinguish "waiting for user" from "handler failed or fell back."

## Design Decisions

1. Selection state lives in `Engine`, not in `intent.DemoHandler`.
   - Reason: the state spans multiple turns and must be cleared when the user answers or changes topic.

2. Selection rendering is deterministic.
   - Reason: this is not the final factual answer; it is a control step. The LLM should not rewrite, reorder, omit, or merge candidates.

3. `resource_info` with no target remains a valid "list resources" query.
   - Reason: PR #61 intentionally made "what machines do I have" work. Do not turn all no-target resource questions into clarification.

4. `monitor_query` with no unique target is the first supported use case.
   - Reason: monitor calls require UHostIds and previous behavior could let the LLM pick or reuse an arbitrary instance.

5. Do not use `TargetRefSlotPosition` in this slice.
   - Reason: it is currently only a schema placeholder; relying on it would create planner/handler drift.

6. Candidate lists and continuation must use the same registry snapshot.
   - Reason: if candidates come from a fresh `DescribeCompShareInstance` call but the next turn resolves against an older registry snapshot, the selected instance may fail to resolve. The pending state must therefore store the immutable `entity.RegistrySnapshot` used to render the candidate list.

7. Do not bump `trace.v0.2`.
   - Reason: this slice only needs a new `cutover_status` enum value. The existing planner trace field is enough; adding new trace fields would create unnecessary schema churn.

---

## Task 1: Pending Selection Types and Renderer

**Files:**
- Create: `internal/engine/resource_selection.go`
- Test: `internal/engine/resource_selection_test.go`

**Step 1: Write failing tests**

Add tests for:

- Rendering two candidates includes ordinal, ID, name, state, GPU, CPU, memory, zone, and a selection instruction.
- Rendering duplicate names includes IDs for both and does not merge them.
- Parsing `1`, `第2个`, `选第二台`, exact UHostId, and exact name resolves to the expected candidate.
- Invalid selection returns no match and does not clear state.
- Ambiguous exact name among duplicate candidates returns no match and keeps asking.

When testing Chinese selection phrases, write the literals directly in UTF-8 Go source or use Unicode escapes in the test. Do not copy mojibake text from a terminal transcript.

Run:

```powershell
go test ./internal/engine -run "ResourceSelection" -count=1
```

Expected: FAIL because the types/functions do not exist.

**Step 2: Implement minimal types**

Add unexported engine-local types:

```go
type pendingResourceSelection struct {
    originalUserMsg string
    plan            intent.Plan
    snapshot        entity.RegistrySnapshot
    candidates      []entity.InstanceSnapshot
    createdTurn     int
    invalidAttempts int
}

type resourceSelectionMatch struct {
    instance entity.InstanceSnapshot
    ok       bool
    ambiguous bool
}
```

Add helpers:

- `renderResourceSelectionPrompt(p pendingResourceSelection) string`
- `matchResourceSelection(input string, p pendingResourceSelection) resourceSelectionMatch`
- `isResourceSelectionExpired(currentTurn int, p pendingResourceSelection) bool`

The expiry rule for v1:

- `createdTurn` is the turn where the selection prompt was shown.
- `createdTurn + 1`: if the user gives a valid selection, continue; if invalid, increment `invalidAttempts`, repeat the same prompt, and keep pending.
- `createdTurn + 2`: if the user gives a valid selection, continue; if invalid, clear pending and let the current message go through normal routing.
- Any later turn: clear pending before normal routing.

This prevents stale pending state from hijacking unrelated conversations while still giving the user one correction attempt.

**Step 3: Run tests**

```powershell
go test ./internal/engine -run "ResourceSelection" -count=1
```

Expected: PASS.

**Step 4: Commit**

```powershell
git add internal/engine/resource_selection.go internal/engine/resource_selection_test.go
git commit -m "feat: add resource selection state helpers"
```

---

## Task 2: Candidate Discovery for Monitor Queries

**Files:**
- Modify: `internal/engine/engine.go`
- Test: `internal/engine/engine_test.go`

**Step 1: Write failing tests**

Add tests that exercise Phase 1 cutover with a scripted planner and a mock executor:

1. `monitor_query` with no target:
   - `DescribeCompShareInstance` is called to build candidates.
   - `GetCompShareInstanceMonitor` is not called.
   - reply contains a selection prompt.
   - `pendingResourceSelection` is set.
   - planner trace uses `cutover_status="selection_required"`.

2. `monitor_query` with ambiguous name:
   - no monitor call.
   - reply lists the ambiguous candidates.

3. Single candidate:
   - the engine should continue directly and call `GetCompShareInstanceMonitor`.
   - no pending selection remains.

4. Explicit all-account phrasing remains out of scope:
   - if planner emits `resource_info` with no target, existing list behavior remains unchanged.

Run:

```powershell
go test ./internal/engine -run "ResourceSelection|Phase1.*Monitor" -count=1
```

Expected: FAIL.

**Step 2: Add engine state**

In `Engine`, add:

```go
pendingResourceSelection *pendingResourceSelection
```

Do not expose this outside `internal/engine`.

**Step 3: Add candidate builder**

Add helper methods in `engine.go` or `resource_selection.go`:

- `buildResourceSelectionForPlan(ctx context.Context, result intent.PlannerResult, snapshot entity.RegistrySnapshot, onStep func(StepEvent)) (*pendingResourceSelection, bool)`
- `candidateInstancesForSelection(ctx context.Context, plan intent.Plan, snapshot entity.RegistrySnapshot, onStep func(StepEvent)) ([]entity.InstanceSnapshot, bool)`

Rules:

- If the registry snapshot is missing or stale, first call existing `refreshRegistry(ctx, entity.RefreshReasonTTL)` / `refreshRegistry(ctx, entity.RefreshReasonManual)` through the safe path, then use the refreshed `e.RegistrySnapshot()` as the source of candidates.
- For `monitor_query` with zero `TargetRefs`, list candidate instances from the current non-stale registry snapshot.
- Store the same immutable snapshot in `pendingResourceSelection.snapshot`.
- For ambiguous name, use registry `ResolveByName` matches.
- Do not use monitor API to discover candidates.
- Limit prompt candidates to 20. If more exist, render the first 20 sorted by ID and tell the user to narrow by name/ID. Do not silently choose one.

**Step 4: Hook into cutover**

In `tryPhase1Cutover`, before falling back to ReAct for `FallbackMissingTarget`, `FallbackAmbiguousTarget`, or `FallbackUnresolvedTarget` on `monitor_query`, create and store pending selection.

Return the deterministic selection prompt as handled. Do not call grounded renderer for selection prompts.

Add `intent.CutoverStatusSelectionRequired = "selection_required"` and emit it for this path.

**Step 5: Run tests**

```powershell
go test ./internal/engine -run "ResourceSelection|Phase1.*Monitor" -count=1
```

Expected: PASS.

**Step 6: Commit**

```powershell
git add internal/engine/engine.go internal/engine/resource_selection.go internal/engine/*_test.go
git commit -m "feat: ask for instance selection before monitor queries"
```

---

## Task 3: Resume Original Intent After User Selection

**Files:**
- Modify: `internal/engine/engine.go`
- Modify: `internal/engine/resource_selection.go`
- Test: `internal/engine/engine_test.go`

**Step 1: Write failing tests**

Add tests for the full two-turn flow:

1. Turn 1: `CPU 高怎么办` -> selection prompt, no monitor call.
2. Turn 2: `选第二台` -> engine calls `GetCompShareInstanceMonitor` with the second candidate ID.
3. Turn 2 exact UHostId -> engine calls monitor for that ID.
4. Turn 2 duplicate name -> engine asks for a more specific selection and does not call monitor.
5. Turn 2 unrelated text -> engine repeats the selection prompt once and does not call LLM/ReAct.
6. After stale invalid selection, pending clears and normal routing resumes on the following turn.

Run:

```powershell
go test ./internal/engine -run "ResourceSelectionContinuation" -count=1
```

Expected: FAIL.

**Step 2: Handle pending selection at Chat entry**

At the start of `Engine.Chat`, after account-billing hard-block and before planner dispatch, check `e.pendingResourceSelection`.

If present:

- Try to match the user's selection.
- If valid, copy the stored plan and replace `Slots.TargetRefs` with one `TargetRefUHostIDUserInput` using the selected ID and `SourcePriorTurn`.
- Set `SourceSpan` to the selected UHostId because the selection prompt from the prior assistant turn contains that exact ID. This keeps the internal plan compatible with existing provenance rules even if a future refactor re-validates resumed plans.
- Run the resumed handler with `pendingResourceSelection.snapshot`, not a freshly captured unrelated snapshot.
- Clear pending.
- Execute the original intent through the same Phase 1 handler path.
- If invalid, return the deterministic selection prompt and keep/expire pending according to Task 1 rules.

Important: append assistant messages consistently so conversation history remains coherent.

**Step 3: Preserve original metrics/time window**

When resuming monitor query, preserve:

- `Slots.Metrics`
- `Slots.TimeWindow`
- `RequiredTools`
- `Confidence`

Only replace the target.

**Step 4: Run tests**

```powershell
go test ./internal/engine -run "ResourceSelectionContinuation" -count=1
```

Expected: PASS.

**Step 5: Commit**

```powershell
git add internal/engine/engine.go internal/engine/resource_selection.go internal/engine/*_test.go
git commit -m "feat: resume monitor query after resource selection"
```

---

## Task 4: Trace and CLI Smoke Documentation

**Files:**
- Modify: `docs/plans/2026-05-12-resource-selection-clarification.md`
- Optional test: `cmd/trace_test.go` only if trace behavior needs a small unit assertion.

**Step 1: Verify trace behavior**

Run a CLI smoke with:

```powershell
go test ./... -count=1
python scripts/test_planner_vs_guard_diff.py
git diff --check
powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\secret_scan.ps1
```

Also run the existing PR #61 regression surface explicitly:

```powershell
go test ./internal/intent -run "ResourceInfoHandler_Filter|ResourceFilter|Planner" -count=1
go test ./internal/renderer -run "ResourceInfo" -count=1
go test ./internal/engine -run "Phase1ResourceInfo|GroundedRenderer" -count=1
```

Then run a real-account manual smoke with deepseek-v4-flash:

```text
CPU 高怎么办？
选第二台
qa-shadow-20260417-4090 当前 CPU、内存、GPU、显存使用率是多少？
我现在有哪些机器？
```

Expected:

- First turn asks the user to choose an instance.
- Second turn calls monitor for the selected candidate.
- Direct named monitor query still works.
- Resource list from PR #61 still works.

**Step 2: Record artifact**

If a real-account smoke is run, add a redacted summary artifact under:

```text
eval/shadow_qa/<date>-resource-selection-smoke/README.md
```

Do not commit raw transcript, raw trace, IP addresses, public/private keys, bearer tokens, or account financial values.

**Step 3: Commit**

```powershell
git add docs/plans/2026-05-12-resource-selection-clarification.md eval/shadow_qa/<date>-resource-selection-smoke/README.md
git commit -m "docs: record resource selection smoke"
```

---

## Review Checklist

- [ ] No IP reverse lookup was added.
- [ ] No SSH or remote command path was added.
- [ ] `resource_info` with no target still lists resources.
- [ ] PR #61 resource filters still work: running/stopped/gpu/AND filter tests pass.
- [ ] PR #61 grounded renderer validation still prevents wrong counts and missing IDs.
- [ ] `monitor_query` missing target does not fall back to ReAct.
- [ ] Selection-required turns emit `cutover_status="selection_required"`.
- [ ] The agent never chooses the first candidate by default.
- [ ] Duplicate names require ID or ordinal selection.
- [ ] Selection prompt contains IDs to disambiguate.
- [ ] Selection continuation uses the same immutable registry snapshot that rendered the candidates.
- [ ] Grounded renderer is not used for selection prompts.
- [ ] Existing #61 resource filters still pass.
- [ ] `go test ./... -count=1` passes.

## Manual Test Questions

Use these after implementation:

```text
CPU 高怎么办？
选第二台
看下这台 GPU 忙不忙
qa-shadow-20260417-4090 当前 CPU、内存、GPU、显存使用率是多少？
我现在有哪些机器？
哪些 4090 机器正在运行？
117.50.121.35 无法连接怎么办？
```

Expected for IP question in this slice: honest unsupported answer or fallback to general non-instance-internal guidance; no IP-to-instance lookup and no SSH/internal command attempt.
