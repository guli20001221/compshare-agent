# ReAct Prompt Debloat And Cardization Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Reduce duplicated ReAct prompt rules, generate action guidance from code-owned registries, and add prompt-change gates so future prompt edits ship with golden coverage.

**Architecture:** Preserve the existing three runtime forms: `routing`, `terminal_rag`, and `agent`. Keep planner routing registry generation and planner SHA pinning as-is. Refactor only the ReAct system prompt path so it composes shared base rules plus small intent/runtime-specific cards; safety remains enforced by code gates, not prose.

**Tech Stack:** Go, PowerShell smoke scripts, JSONL trace, existing `internal/prompt`, `internal/engine`, `internal/intent`, `internal/tools`, `internal/workflow`, `internal/diagnosis`, and `eval`.

---

## Current State Snapshot

Verified on 2026-06-05 in `F:\compshare-agent`.

- ReAct already has two prompt modes: mutating and read-only. `prompt.BuildSystemWithOptions` switches only on `MutatingToolsEnabled`.
- Planner prompt is already strongly gated: `buildSystemPrompt` appends generated routing fragments and `systemPromptSHA256Baseline` pins the full planner prompt.
- Routing is already registry-generated: `internal/routing/registry_gen.go` contains 10 generated routes, matching `routingIntentOrder`.
- ReAct prompt still handwrites workflow and diagnosis selection rules. The current hand-written lists are not drifted today: 13 workflow names and 6 diagnosis names match their registries and operation tool subset.
- ReAct prompt tests are weaker than planner tests: they mostly assert contains/not-contains strings, not full prompt drift or golden behavior.
- Tool exposure is already scoped at runtime: ReAct passes `tools.VisibleRegistryForSubset(intent.IntentToolSubset(...), mutatingEnabled)`.
- Safety already lives in code: L2 actions are refused, mutating actions require confirmation unless they are inside workflow/diagnosis internal origins, and read-only mode hides mutating workflow tools.

Baseline checks already passed:

```powershell
$env:COMPSHARE_PROJECT_ID='proj-test'
go test ./internal/prompt ./internal/intent ./internal/workflow ./internal/diagnosis ./internal/tools ./internal/security -count=1
go test ./internal/engine ./eval/runtime_form -run "Workflow|ToolSubset|Mutating|Prompt|Diagnosis|RuntimeForm|Dispatch|Confirm|Visible|ExecuteWorkflow" -count=1
```

## Non-Goals

- Do not rewrite the planner prompt in this work.
- Do not change workflow behavior, diagnosis behavior, RAG corpus, or qwen sidecars.
- Do not relax destructive-action refusal or confirmation gates.
- Do not move all prompt rules into code; only code-enforce safety/correctness boundaries. Keep model-behavior guidance as prompt cards when code cannot enforce it.

## PR Order

1. PR-1: Add ReAct prompt gates and registry parity tests.
2. PR-2: Extract duplicated shared prompt prose with byte-identical output.
3. PR-3: Generate workflow and diagnosis cards from code-owned registries.
4. PR-4: Inject intent-scoped ReAct cards only when ReAct fallback actually runs.
5. PR-5: Add golden prompt/trace cases and prompt-change checklist.
6. PR-6: Remove obsolete hand-written lists and shrink final prompt variants.
7. PR-7: Rollout, compare traces, then default-on only after evidence.

**Byte-stability boundary (critical):** PR-1 and PR-2 are **byte-stable** — the ReAct prompt SHA must not move; that is their acceptance gate. PR-3 is the **first PR that intentionally changes ReAct prompt bytes**, and PR-4 and PR-6 also change bytes. Every byte-changing PR (PR-3, PR-4, PR-6) MUST update the PR-1 snapshot baseline in-commit with a justification AND ship a golden parity case. Do not mix a byte-changing edit into a PR that claims zero behavior change.

---

## PR-1: ReAct Prompt Gates

**Purpose:** Add guardrails before changing prompt structure.

**Files:**

- Create: `internal/prompt/react_prompt_snapshot_test.go`
- Create: `internal/prompt/prompt_registry_parity_test.go`
- Modify only if needed: `internal/prompt/builder_test.go`

**Steps:**

1. Add SHA tests for current mutating and read-only ReAct prompts using fixed context:

   ```go
   prompt.BuildSystemWithOptions("test context", prompt.BuildOptions{MutatingToolsEnabled: true})
   prompt.BuildSystemWithOptions("test context", prompt.BuildOptions{MutatingToolsEnabled: false})
   ```

2. Add registry parity tests:
   - workflow registry names equal workflow tool names exposed in `tools.Registry`
   - workflow registry names equal `IntentOperationLifecycle` subset workflow names
   - diagnosis registry names equal diagnosis tool names in `tools.Registry`
   - diagnosis registry names equal `IntentDiagnosis` subset diagnosis names

3. Add prompt presence tests for current generated/handwritten surface:
   - mutating prompt contains all registered workflows
   - read-only prompt contains no workflow names
   - both modes contain all diagnosis names

4. Run:

   ```powershell
   go test ./internal/prompt ./internal/intent ./internal/tools ./internal/workflow ./internal/diagnosis -count=1
   ```

**Acceptance:**

- Tests pass without behavior changes.
- Any future ReAct prompt text drift fails loudly unless the baseline is intentionally updated.

---

## PR-2: Extract Shared Prose

**Purpose:** Remove obvious copy-paste without changing final prompt bytes.

**Files:**

- Modify: `internal/prompt/segments.go`
- Modify: `internal/prompt/segment_operation.go`
- Modify: `internal/prompt/segment_readonly.go`
- Test: `internal/prompt/react_prompt_snapshot_test.go`

**Byte-stability reality (verified 2026-06-05, char-by-char):** the four "shared" pieces are NOT uniformly identical across modes. Only one can be extracted with zero byte change. PR-2 extracts ONLY byte-identical spans; anything with prefix/suffix/wording divergence is deferred to an intentional-change PR.

| Piece | Mutating | Read-only | Byte-identical? | PR-2 action |
|---|---|---|---|---|
| optional-repair command boundary | `segment_operation.go:83` | `segment_readonly.go:13` | ✅ yes | Extract to shared constant |
| read-only self-check command | `segment_operation.go:82` (has leading `诊断类回答`) | `segment_readonly.go:12` (no prefix) | ⚠️ inner span only | Extract inner span only IF byte-stable as `prefix+CONST`; else defer |
| complete-listing rule | `segment_operation.go:91` (no trailing `。`) | `segment_readonly.go:27` (trailing `。`) | ⚠️ differ by `。` | Extract only if expressible as `CONST` / `CONST+"。"` with zero net change; else defer |
| delete/destroy refusal | `segment_operation.go:84` `删除/销毁操作拒绝执行，引导用户去控制台手动操作` | `segment_readonly.go:14` `删除/销毁类操作始终拒绝执行，并引导用户到控制台手动操作。` | ❌ no (`类`/`始终`/`并`/`去`→`到`/`。`) | DO NOT unify in PR-2. Leave divergent, or unify as a deliberate prompt change in PR-6 with snapshot+golden |

**Steps:**

1. Define shared constants in `segments.go` ONLY for spans that are byte-identical (optional-repair) or extractable as `prefix+CONST` / `CONST+suffix` with provably zero net change.
2. Replace those duplicated spans in mutating and read-only segments with the shared constants.
3. Keep exact whitespace stable; do not improve or unify wording in this PR. Divergent pieces (delete/destroy refusal, and any span that cannot be made byte-stable) stay as-is and move to a later intentional-change PR.
4. Run:

   ```powershell
   go test ./internal/prompt -run "ReAct|BuildSystem|CompleteListing|ReadOnly" -count=1
   ```

**Acceptance:**

- Mutating and read-only ReAct prompt SHA values stay **unchanged** (this is the gate that proves PR-2 changed zero bytes).
- The delete/destroy refusal divergence is explicitly left intact (not silently unified).
- No routing, workflow, or diagnosis tests change.

---

## PR-3: Generate Workflow And Diagnosis Cards

**Purpose:** Stop maintaining workflow and diagnosis lists twice.

**Files:**

- Create: `internal/prompt/cards.go`
- Create or modify: `internal/prompt/cards_test.go`
- Modify: `internal/workflow/registry.go`
- Modify: `internal/diagnosis/registry.go`
- Modify if needed: `internal/tools/registry.go`
- Modify: `internal/prompt/segment_operation.go`
- Modify: `internal/prompt/segment_readonly.go`

**⚠️ This is the FIRST PR that intentionally changes ReAct prompt bytes.** PR-1/PR-2 are byte-stable; replacing handwritten lists with generated cards WILL move the mutating/read-only ReAct prompt SHA. Therefore PR-3 MUST: (a) update the PR-1 snapshot baseline in the same commit with a justification, and (b) ship a golden case (PR-5 style) proving intent/tool/boundary parity. Do NOT mix PR-3 with any "zero behavior change" claim — mirror how the planner side already gates via `systemPromptSHA256Baseline`.

**Scope boundary — what is and isn't generated:**

- GENERATE (catalog info only): the **bare name + short one-line description** of each workflow/diagnosis action, sourced from registry name + `tools.Registry` function description.
- DO NOT generate / keep hand-authored: every trigger-phrase mapping, parameter semantic, and behavior rule that surrounds the lists. These are NOT derivable from registry keys or tool descriptions. Verified examples that must stay verbatim (`segment_operation.go:20-50`):
  - `:25` 创建失败（如售罄）后不要自动重试其他 GPU，告知用户由其决定
  - `:36` CreateDiskWorkflow 必须带 Size（GB），用户没说容量时先追问，不要进入确认；新建盘≠扩盘
  - `:37` ResizeDiskWorkflow：Size 是目标容量不是新增容量；扩系统盘传 DiskType=Boot；扩多块中的某块必须传 DiskId
  - `:38` CreateCustomImageWorkflow 未提供 Name 时先追问，不要直接调 raw `CreateCompShareCustomImage`，不发布社区镜像
  - `:39` vague_failure 触发词清单（跑崩了/挂了/不对劲…）→ 先追问实例+症状
  - `:35` 重装前先 DescribeCompShareImages / DescribeCommunityImages 查目标镜像 ID

**Design:**

- Add stable exported registry helpers:
  - `workflow.RegisteredWorkflowActions() []string`
  - `diagnosis.RegisteredDiagnosisActions() []string`
- Keep a stable human order. Do not iterate maps directly.
- Build short cards = registry name + `tools.Registry` function description ONLY.
- Keep all non-derivable rules as hand-authored cards (see Scope boundary above), including:
  - no text before `*Workflow` call (see also `segment_operation.go:94`)
  - vague failure asks for instance and symptom before diagnosis
  - knowledge questions must use RAG/tools, not memory
  - all per-workflow trigger phrases and parameter semantics listed above

**Steps:**

1. Add failing tests proving generated cards include all registered workflows and diagnosis tools.
2. Add stable action-order helpers in workflow and diagnosis packages.
3. Build `renderWorkflowSelectionCard()` and `renderDiagnosisSelectionCard()`.
4. Replace handwritten action lists in ReAct prompt with generated cards.
5. Run:

   ```powershell
   go test ./internal/prompt ./internal/workflow ./internal/diagnosis ./internal/tools -count=1
   ```

**Acceptance:**

- Adding a workflow or diagnosis chain requires one registry/tool-description change, not a prompt list edit.
- Parity tests fail if a registry action has no prompt card or no visible tool.

---

## PR-4: Intent-Scoped ReAct Prompt Injection

**Purpose:** Do not show full operation rules to every ReAct fallback turn.

**Files:**

- Modify: `internal/prompt/builder.go`
- Modify: `internal/prompt/cards.go`
- Modify: `internal/engine/engine.go`
- Test: `internal/prompt/cards_test.go`
- Test: `internal/engine/*prompt*_test.go`

**Key detail (verified 2026-06-05):**

`refreshSystemPrompt` (`engine.go:1013`) runs before planner classification, so the persisted `e.messages[0]` cannot be intent-scoped. But `lastPlannerIntentThisTurn` IS set before the ReAct loop (`engine.go:1310/1318`), so the intent-scoped card belongs in the per-call message build, inserted as an ephemeral system message.

**Reuse the existing primitive — do NOT write a new helper and do NOT replace system content:**

- The insert helper already exists: `withEphemeralSystemBeforeLastUser(messages, content)` at `engine.go:3896`, and is already in production use at `engine.go:1115` (it inserts `monitorRecallRequiredToolNote` into `req.Messages`, not `e.messages`). `buildMessagesForLLM` (`engine.go:3863`) uses the same NOT-persisted pattern for `staleStateNote`.
- The intent card MUST be **inserted** before the latest user message via that helper, on the per-call copy. It MUST NOT replace `e.messages[0]`.
- Rationale for insert-not-replace: (a) replacing the system prefix duplicates `BuildSystemWithOptions` and risks drift from the PR-1 snapshot; (b) replacing the prefix every turn busts prompt-prefix caching, whereas inserting before the last user keeps `e.messages[0]` stable.

**Card selection:**

- `operation_lifecycle`: workflow card, confirmation/no-pretext card, stale-state rule, minimal safety card.
- `diagnosis` / `vague_failure`: diagnosis card, read-only command boundary, evidence refresh rule; no workflow card.
- `resource_info` / `monitor_query` / `billing_instance` / `disk_info`: query card, complete-listing rule, real-time fact boundary.
- `recommendation`: recommendation card plus price/stock fact boundary.
- `knowledge_qa`: no ReAct answer from memory; force RAG/available evidence boundary.
- `unknown`: scope/refusal card only; no workflow or diagnosis catalog.
- read-only mode: never include mutating workflow cards.

**Steps:**

1. Reuse `withEphemeralSystemBeforeLastUser` (`engine.go:3896`) — do not author a new helper.
2. Extend `prompt.BuildOptions` with optional intent/runtime/card selection fields (zero-value must reproduce current bytes, protected by the PR-1 snapshot).
3. In the ReAct loop, build the intent card from `lastPlannerIntentThisTurn` and insert it via the helper into the per-call message copy (alongside, and composing with, the existing `staleStateNote` insert path). Insert only — never replace system content.
4. Ensure `e.messages[0]` and the rest of `e.messages` remain unchanged after the call (assert in test).
5. Run:

   ```powershell
   go test ./internal/prompt ./internal/engine -run "Prompt|BuildMessages|IntentScoped|ReadOnly|Workflow|Diagnosis" -count=1
   ```

**Acceptance:**

- Diagnosis fallback prompt contains diagnosis rules and no workflow catalog.
- Operation fallback prompt contains workflow rules and no diagnosis catalog except generic safety if needed.
- Read-only prompt never exposes workflow tools or workflow cards.
- Existing ReAct tool subset behavior remains unchanged.

---

## PR-5: Golden Cases For Prompt Changes

**Purpose:** Make prompt edits measurable, not taste-based.

**Files:**

- Create: `eval/prompt_goldens/cases.json`
- Create: `eval/prompt_goldens/README.md`
- Create or extend: `eval/context_prompt_cli_regression_cases.json`
- Create or extend: `eval/context_prompt_cli_regression.ps1`
- Create: `docs/dev/prompt-change-checklist.md`

**Required cases:**

- knowledge FAQ must not be answered from ReAct memory.
- SSH issue routes to diagnosis and does not become knowledge-only.
- vague failure asks a clarifying question before diagnosis.
- `帮我关机` routes to operation lifecycle and asks/selects target.
- create instance uses `CreateInstanceWorkflow`, not raw create API.
- create disk without size asks for size before confirmation.
- resize existing disk uses `ResizeDiskWorkflow`, not `CreateDiskWorkflow`.
- create custom image without name asks for name and never calls raw `CreateCompShareCustomImage`.
- destroy/delete request never calls destructive APIs.
- read-only mode gives guidance but does not execute workflow.
- listing instances/images/resources does not say "未显示全/剩余 N".

**Checks:**

- expected planner intent
- allowed/forbidden tool actions
- expected runtime form
- schema valid
- no hallucination escape count
- no forbidden destructive action

**Steps:**

1. Add unit-level prompt-card goldens that do not need API keys.
2. Add CLI trace goldens that run only when `LLM_API_KEY` is set.
3. Document rule: prompt PRs must include either a snapshot update plus reason, or a golden case proving no behavior drift.

**Acceptance:**

- Default `go test ./...` covers unit-level prompt/card gates.
- Real CLI script produces a JSON summary and exits non-zero on intent/tool boundary drift.

---

## PR-6: Remove Obsolete Handwritten Lists

**Purpose:** Shrink prompt text after cards and goldens are in place.

**Files:**

- Modify: `internal/prompt/segment_operation.go`
- Modify: `internal/prompt/segment_readonly.go`
- Modify: `internal/prompt/cards.go`
- Test: `internal/prompt/*_test.go`

**Two workflow enumerations exist — handle both, delete neither wholesale:**

- Enumeration 1: the selection-rules list in `segmentMutatingRules` (`segment_operation.go:20-38`). Its bare name list can be replaced by the generated card; its surrounding trigger/param/behavior prose stays (see PR-3 Scope boundary).
- Enumeration 2: the **no-pretext rule** in `segmentMutatingReplyStyle` (`segment_operation.go:94`), which re-lists all 13 workflow names inside `禁止在工具调用前生成任何文本`. This rule is generation-shaping (not code-enforceable) and MUST survive. Only its embedded name list may be sourced from the registry; the rule body stays verbatim.

**Steps:**

1. Replace the bare workflow name catalog in `segment_operation.go:20-38` with the generated card; keep all trigger/param/behavior prose.
2. In `segment_operation.go:94`, source only the name list from the registry; keep the no-pretext rule body intact.
3. Delete the duplicated diagnosis route table where the generated diagnosis card now owns the names; keep diagnosis trigger phrases.
4. Keep only short non-derivable behavior boundaries.
5. Record old/new prompt byte lengths in test output or a small markdown note.
6. Update the PR-1 snapshot baseline (this PR changes bytes) with justification + golden parity.
5. Run:

   ```powershell
   go test ./internal/prompt ./internal/intent ./internal/engine -count=1
   ```

**Acceptance:**

- Prompt variants are smaller.
- All card inclusion/exclusion tests pass.
- Golden cases pass.

---

## PR-7: Rollout And Stop Conditions

**Purpose:** Avoid flipping a prompt architecture change blindly.

**Files:**

- Modify if needed: `cmd/cli.go`
- Modify if needed: `cmd/agent.go`
- Modify if needed: `deploy/conf/agent.yaml`
- Create: `eval/prompt_goldens/run_prompt_rollout.ps1`
- Create: `eval/prompt_goldens/latest_report.md`

**Rollout:**

1. Add `USE_INTENT_SCOPED_REACT_PROMPT=1`, default off.
2. Run old prompt and new prompt against the same golden set.
3. Compare:
   - **prompt token delta (first-class metric)** — intent-scoping shrinks per-turn prompt tokens; this is a primary success axis, not optional. If the trace does not yet record per-turn prompt tokens, add that field as part of this PR so the delta is measurable.
   - intent stability
   - tool actions
   - actual runtime form
   - escaped hallucination count
   - workflow confirmation path
4. Default on only if golden pass rate is unchanged AND the prompt token delta is a measured reduction (not merely "size looks smaller").

**Stop conditions:**

- Any destructive action appears in trace.
- Read-only mode exposes workflow cards/tools.
- Diagnosis cases lose the diagnosis tool path.
- Workflow cases emit pre-confirmation prose before the workflow call.
- Planner prompt SHA changes unexpectedly.

**Final verification command set:**

```powershell
$env:COMPSHARE_PROJECT_ID='proj-test'
go test ./internal/prompt ./internal/intent ./internal/engine ./internal/tools ./internal/workflow ./internal/diagnosis ./eval/runtime_form -count=1
powershell -NoProfile -ExecutionPolicy Bypass -File .\eval\context_prompt_cli_regression.ps1 -Tag prompt-cardization -Mutating 1
powershell -NoProfile -ExecutionPolicy Bypass -File .\eval\context_prompt_cli_regression.ps1 -Tag prompt-cardization-readonly -Mutating 0
```

Run the PowerShell smoke scripts only when `LLM_API_KEY` and smoke credentials are available. If not available, record that live smoke is blocked and rely on unit gates plus existing trace fixtures.

## Recommended First Implementation Slice

Start with PR-1 and PR-2 only. They are low-risk, test-first, and preserve exact behavior. Do not start PR-3 until ReAct prompt snapshot and parity tests are in place.

