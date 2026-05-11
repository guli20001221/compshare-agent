# ResourceQuery Structured Filter Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make resource inventory questions like "which machines are running/stopped" execute as structured resource filters instead of asking the final renderer LLM to infer and count from an unfiltered instance list.

**Architecture:** The planner remains responsible for natural-language understanding and emits a whitelisted `TargetRef{type:"filter"}` value. The resource handler remains responsible for API execution, deterministic filtering, counts, and envelope facts. The grounded renderer only turns already-filtered facts into readable output.

**Tech Stack:** Go, existing `internal/intent` planner/validator/handler/envelope packages, existing grounded renderer and trace plumbing.

---

## Product Boundary

This slice covers instance resource inventory only:

- Running/stopped instance queries.
- GPU type instance queries such as "4090 machines" and "V100S machines".
- Full instance list remains supported.

This slice explicitly does not cover:

- IP-based diagnosis such as `117.50.121.35 无法连接`.
- Instance-internal operations, SSH login, remote commands, or reading files.
- Monitor ranking such as "idle machines"; that needs resource selection plus monitor facts in a later slice.
- Billing answer synthesis; instance billing remains on the existing ReAct / diagnosis path.

## Design Choice

Use generic filter values rather than one-off intent names:

```json
{"type":"filter","value":"state=running"}
{"type":"filter","value":"state=stopped"}
{"type":"filter","value":"gpu_type=4090"}
```

Existing legacy values `all_running` / `all_stopped` may be accepted as aliases for compatibility, but new planner prompt examples should prefer `state=running` and `state=stopped`.

The handler must fetch the instance list once using `DescribeCompShareInstance` and then filter locally. This avoids adding API-specific query semantics before the upstream contract is proven, and it lets the trace/envelope show total vs matched counts.

## Data Contract

For a resource filter query:

- `Plan.Intent == resource_info`
- `Plan.Slots.TargetRefs` contains exactly one or more filter refs, or a name/id target as before.
- Supported filter fields:
  - `state=running`
  - `state=stopped`
  - `gpu_type=<non-empty value>`
- Multiple filter refs use AND semantics.
- Different fields may be combined, e.g. `state=running` AND `gpu_type=4090`.
- Duplicate/conflicting fields are rejected for this slice:
  - `state=running` + `state=stopped` -> fallback/validation error.
  - `gpu_type=4090` + `gpu_type=V100S` -> fallback/validation error.
- Filter refs cannot be mixed with explicit `name` or `uhost_id_user_input` refs in this slice.
- Unsupported filter fields remain validation errors.

Handler output:

- `ToolAction == DescribeCompShareInstance`
- `ToolArgs == {"Limit":100}` for filters and full-list queries.
- `result.Envelope.Subjects` contains only matched instances.
- `result.Envelope.Computed` includes:
  - `filter_applied` with normalized filter expression(s)
  - `matched_count`
  - `total_count`
- Deterministic fallback reply lists only matched instances and includes ID + name + state.

Renderer safety:

- Existing grounded renderer validator remains unchanged unless tests prove it blocks new computed facts.
- Renderer must not be responsible for filtering or counting.

## Task 1: Planner And Validator Contract

**Files:**

- Create: `internal/intent/resource_filter.go`
- Test: `internal/intent/resource_filter_test.go`
- Modify: `internal/intent/validator.go`
- Modify: `internal/intent/planner.go`
- Modify: `internal/intent/validator_test.go`
- Modify: `internal/intent/planner_prompt_test.go`
- Modify: `eval/intent/fixtures.jsonl`
- Modify if needed: `eval/intent/offline_eval_test.go`

**Steps:**

1. Add failing validator tests:
   - accepts `TargetRef{Type: TargetRefFilter, Value: "state=running"}`
   - accepts `TargetRef{Type: TargetRefFilter, Value: "state=stopped"}`
   - accepts `TargetRef{Type: TargetRefFilter, Value: "gpu_type=4090"}`
   - rejects `state=deleted`, `state=running;rm`, `charge_type=dynamic` for this slice.
2. Add one shared parser:
   - `ParseResourceFilter(value string) (ResourceFilter, error)`
   - `ParseResourceFilters(refs []TargetRef) (ResourceFilterSet, error)`
   - validator and handler must both call this parser; do not duplicate string parsing.
3. Extend `validFilterRef` minimally by delegating to the shared parser:
   - normalize lowercase for field names and state values.
   - accept existing `all_running` and `all_stopped` as aliases.
   - accept `gpu_type=<value>` only when `<value>` is alphanumeric plus `-`, `_`, or `.`.
4. Define multi-filter semantics in parser tests:
   - accepts `state=running + gpu_type=4090`.
   - rejects `state=running + state=stopped`.
   - rejects `gpu_type=4090 + gpu_type=V100S`.
5. Update planner prompt examples:
   - "which machines are running" -> `state=running`.
   - "which machines are stopped" -> `state=stopped`.
   - "which 4090 machines do I have" -> `gpu_type=4090`.
6. Add offline eval fixtures so planner behavior is not only manually smoked:
   - `我现在有哪些机器在跑？` -> `resource_info` + `target_refs=[{type:"filter", value:"state=running"}]`
   - `哪些机器已经关机？` -> `resource_info` + `target_refs=[{type:"filter", value:"state=stopped"}]`
   - `哪些是 4090？` -> `resource_info` + `target_refs=[{type:"filter", value:"gpu_type=4090"}]`
   - `哪些 4090 机器正在运行？` -> `resource_info` + both `state=running` and `gpu_type=4090`
7. Ensure offline eval asserts expected filter refs when fixture `expected.plan.slots.target_refs` is present. If already covered by existing target ref assertions, add only fixtures.
8. Run:
   - `go test ./internal/intent -run "TestValidatePlan|TestPlannerPrompt" -count=1`
   - `go test ./eval/intent -count=1`

## Task 2: Deterministic Resource Filtering

**Files:**

- Modify: `internal/intent/handler.go`
- Modify: `internal/intent/handler_resource_test.go`

**Steps:**

1. Add failing handler tests:
   - `state=running` filters out `Stopped` and `Initializing`.
   - `state=stopped` returns only stopped instances.
   - duplicate instance names do not affect count or inclusion because identity is `UHostId`.
   - `gpu_type=4090` returns only matching GPU type and includes both ID and name in fallback reply.
   - `state=running + gpu_type=4090` returns only instances matching both filters.
   - `state=running + state=stopped` returns `FallbackValidation` before the tool call.
   - `state=running + name/id` returns `FallbackValidation` before the tool call.
2. Implement a small `resourceFiltersFromRefs` helper using the shared parser:
   - separates filter refs from name/id refs.
   - this slice rejects mixing filters with explicit name/id refs as `FallbackValidation` to avoid ambiguous semantics.
3. Implement `applyResourceFilters(instances, filterSet)`:
   - field comparisons are case-insensitive for `state`, exact case-insensitive for `gpu_type`.
   - multiple fields are ANDed.
   - sort output by `UHostId` after filtering.
4. Preserve existing name/id target behavior.
5. Run:
   - `go test ./internal/intent -run ResourceInfoHandler -count=1`

## Task 3: Envelope Counts And Grounded Rendering Inputs

**Files:**

- Modify: `internal/intent/envelope.go`
- Modify: `internal/intent/envelope_test.go`
- Modify: `internal/engine/engine_test.go` if existing grounded renderer expectations need the new computed facts.

**Steps:**

1. Add failing envelope tests:
   - filtered resource envelope has `filter_applied`.
   - filtered resource envelope has `matched_count`.
   - filtered resource envelope has `total_count`.
   - `Subjects` contains only matched instances.
2. Change `BuildResourceEnvelope` signature or add `BuildFilteredResourceEnvelope` to avoid breaking monitor code:
   - preferred minimal option: add `BuildResourceEnvelopeWithMeta(instances, ResourceEnvelopeMeta)`.
   - keep `BuildResourceEnvelope(instances)` as a wrapper for existing tests.
3. Include computed facts with `Source: computed`.
4. Ensure deterministic fallback reply includes count line, e.g.:
   - `匹配实例数=2，总实例数=3`
   - table/list lines still include `实例ID` and `名称`.
5. Run:
   - `go test ./internal/intent ./internal/engine ./internal/renderer -count=1`

## Task 3b: Engine Cutover Integration Test

**Files:**

- Modify: `internal/engine/engine_test.go`

**Steps:**

1. Add a failing test for the user-observed bug:
   - planner returns `resource_info` with `TargetRef{Type: filter, Value: "state=running"}`.
   - mock `DescribeCompShareInstance` returns Running + Stopped + duplicate names.
   - grounded renderer is enabled.
   - assert grounded renderer receives an envelope whose `Subjects` contain only Running instance IDs.
   - assert envelope `Computed` includes `filter_applied`, `matched_count`, and `total_count`.
2. Add a second assertion or subtest for `state=running + gpu_type=4090` if the first test fixture already has GPU diversity.
3. Run:
   - `go test ./internal/engine -run "Phase1Cutover.*Resource|ResourceFilter" -count=1`

## Task 4: Verification And Manual Smoke

**Files:**

- Add optional artifact only if a real-account smoke is run:
  - `eval/shadow_qa/2026-05-11-resource-query-filter-smoke.md`

**Steps:**

1. Run full verification:
   - `go test ./... -count=1`
   - `python scripts/test_planner_vs_guard_diff.py`
   - `powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\secret_scan.ps1`
   - `git diff --check origin/main...HEAD`
2. Manual CLI smoke with latest command and:
   - `我现在有哪些机器在跑？`
   - `哪些机器已经关机？`
   - `哪些是 4090？`
3. Expected smoke behavior:
   - running/stopped answers list only matching instances.
   - answer includes ID and name.
   - count equals number of listed unique `UHostId` rows.
   - IP diagnosis question should not be falsely claimed as supported by this path.

## Review Gates

- Plan review by subagent before code.
- Code review by a fresh subagent before PR/merge.
- No code review finding at P0/P1/P2 may remain unresolved.
