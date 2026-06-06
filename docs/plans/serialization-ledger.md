# Serialization Ledger — planner-prompt / PlannerTrace / lane-trace

Status: ACTIVE · created 2026-06-06 · base `origin/main` = `293f944`
Owner doc for the unified plan `docs/plans/2026-06-06-agentic-rag-diagnosis-unified-plan.md` (P0 deliverable).

Purpose: pin the shared, contention-prone serialization surfaces so the P0→P6 arc (and any
concurrent PR) never double-moves the SHA-pinned planner prompt or the `PlannerTrace` schema, and
so already-shipped work (LR0/`#127`) is not re-built. This is a **docs-only** artifact; it changes
no runtime behavior.

---

## 1. LR0 / `#127` is DONE in code — do NOT rebuild

`ActualRuntimeForm = DeriveActualRuntimeForm()` is set at Finish in **both** trace recorders:

- `cmd/trace.go:1066` — `r.record.ActualRuntimeForm = r.record.DeriveActualRuntimeForm()`
- `internal/httpapi/trace_recorder.go:289` — same line (HTTP path parity)

`DeriveActualRuntimeForm()` lives in `internal/observability/trace.go` and returns the
observability **string** consts (`RuntimeFormAgent` / `RuntimeFormTerminalRAG` / `RuntimeFormRouting`,
`trace.go:230-231`) — **distinct** from `intent.RuntimeForm` (typed string, `runtime_form.go:3`).
Tested in `internal/observability/trace_test.go` and `eval/runtime_form/runtime_form_matrix_test.go`.

**Tracker reconciliation (this ledger's job):**
- `#127` (LR0, observe-only lane trace) → mark **DONE** with the file:line evidence above. The
  tracker previously showed it `[pending]`; that drift is exactly what triggers a double-build.
- `#128` (LR1, planner-emits-lane) and `#97` (B4b, planner-emits-task_tier) stay **DEFERRED**, and
  their deferral notes reference the shipped LR0 above. Both change the planner **output schema** +
  `PlannerTrace` + SHA, collide with each other and with P6, and must be serialized into ONE
  coordinated migration **after** P6's `core_shadow` settles — never run in parallel.

The residual value of `#127` is the **P2 measurement harness** (reuse `DeriveActualRuntimeForm`,
do not re-wire it).

---

## 2. SHA-pinned planner prompt — the serialization forcing function

`systemPromptSHA256Baseline = 43afce1977eaff313d8b71a4b672741c5f111b0210c4f7e2e8b65fb717ef2109`
(`internal/intent/planner_examples_test.go:272`).

Any byte change to `buildSystemPrompt()` fails `TestPlannerExamples_FullSystemPromptStable`.
The prompt is assembled from planner examples **and** registry-derived `RoutingPromptFragments()`
(`planner.go:621`) — so an internal rename that changes a route's `planner_directives`/`planner_examples`
emit wording **also** moves the SHA (lesson from `#217`). Keep renames byte-stable where possible.

### Tests that gate `buildSystemPrompt()` (must move together, in the SAME commit, when the prompt changes)
- `internal/intent/planner_examples_test.go:272` — `systemPromptSHA256Baseline` (the SHA).
- `internal/intent/planner_prompt_test.go:86` — `len(examples) == 37 + routeExampleCount` (example partition).
- `internal/intent/planner_prompt_test.go:189` — `total == 56` (exact-equality legacy example count).
- `internal/intent/planner_prompt_test.go:192-214` — `expectedCounts` per-intent map (e.g. `IntentDiagnosis: 4`, `IntentDiskInfo: 4`, `IntentDeployModel: 4`); per-intent exact-equality.
- `internal/intent/runtime_form_test.go` — runtime-form partition tests.
- `internal/intent/tool_subset_test.go` — `TestIntentToolSubset_AllRuntimeIntentsCovered` subset coverage.
- `eval/runtime_form/runtime_form_matrix_test.go`, `eval/trace_gate/` — trace fixtures.

### Rule
**Only ONE open PR at a time may change `buildSystemPrompt()`.** In the unified plan the only two
prompt-changing phases are **P1** (image-list source collapse) and **P4b** (`#123` symptom→agent
boundary). They MUST NOT run concurrently; **order P1 before P4b**. P3/P4a/P5 touch no prompt; P6
stays in `full` mode (SHA-neutral). When the prompt changes, the SHA test and the exact-count tests
(`planner_prompt_test.go:189`/`:204`) move in the same commit with a justified message.

---

## 3. `PlannerTrace` migration slot — RESERVED and left UNSPENT by this plan

Three pending efforts all want planner-output + `PlannerTrace` + SHA changes and would each bump
`SchemaVersion` independently:
- P6 core-projection (this plan) — adds **only** `core_shadow` observe-only fields.
- `#97` / B4b task_tier emission (reserved empty `TraceRecord.TaskTier`).
- `#128` / LR1 lane emission.

**This plan reserves ONE `PlannerTrace` migration slot and leaves it UNSPENT.** P6 adds observe-only
shadow fields under that reserved slot; `#97` and `#128` are deferred and must be serialized into ONE
coordinated migration after P6 — never parallel.

`PlannerTrace`-gating tests: `internal/observability/trace_test.go`, `internal/observability/step_trace_test.go`,
`internal/intent/trace_projection_test.go`, `internal/observability/mysql_writer_test.go`.

---

## 4. Known debt — RECORDED, not actioned in this plan

- **`IntentDiskInfo` planned-vs-actual runtime-form mismatch.** `IntentDiskInfo` is absent from
  `PlannedRuntimeFormForIntent` (`internal/intent/runtime_form.go`) ⇒ defaults to
  `RuntimeFormAgent`, yet its tool subset is a single routing-shaped read
  (`DescribeCompShareInstance`). Genuine planned-vs-actual mismatch. **P6 does NOT touch
  `PlannedRuntimeFormForIntent`**, so this is preserved as-is; an implementer must NOT "fix" it
  mid-P6 (would break byte-stability). Correcting it is a separate prompt/dispatch change outside
  this plan.
- **`IntentMixedDiagnosisKB` / `IntentMixedBillingKB` dead constants** (`internal/intent/types.go:19-20`,
  in `RuntimeIntents()` `:196-197`). Still enumerated; P6's reverse projection MUST give each an
  explicit reverse entry (or the round-trip throws) but does NOT delete them. Cleanup deferred.

---

## 5. External-KB default-off invariant

`COMPSHARE_EXTERNAL_KNOWLEDGE` stays **default-off** through P0–P4b. The agentic-gate flip (P5) is
decoupled and turns on **platform** RAG only. The external default-on flip is a separate one-line
decision gated on the P0 Top-3 per-group parity result. No PR in P0–P6 silently flips it.
