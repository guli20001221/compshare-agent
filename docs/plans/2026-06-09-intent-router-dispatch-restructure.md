# Intent-Router / Dispatch-Contract Restructure Plan

**Date:** 2026-06-09
**Status:** PLAN (post-#251 agentic-RAG flip). Authored after two adversarial review workflows + lead's 5 corrections.
**Predecessors:** `docs/plans/2026-06-05-planner-classification-simplification-plan.md`, `docs/research/naming-remediation-plan.md`, `docs/adr/001-task-tier-architecture.md`.
**ADR target:** `docs/adr/009-intent-router-dispatch-contract.md`.
**Authoritative copy:** this file is the single authoritative plan. The baseline SHA must be **re-read from `origin/main` before PR1**. Older planning notes carrying `43afce19…` (e.g. `2026-06-06-agentic-rag-diagnosis-unified-plan.md`, the LR0 serialization ledger) are **superseded** by this file — `64dc6a4c…` is current (see §5 PR1, §7).

> Line numbers below are as-verified in this session against `origin/main` and the #251 branch. They MUST be re-confirmed against post-#251-merge `main` before each PR (rule: "post-merge verify authoritative main"). They are anchors, not contracts.

---

## 1. The problem — not "missing a planner", but "the Router is misnamed and routing ownership is scattered"

The thing we call **`Planner`** is, by every structural test, an **intent Router**:

- `Plan` (`internal/intent/types.go:109-123`) is a **flat classification object** — `{Intent, Scope, Skills, Slots, RequiredTools, Retrieval, HardBlockHint, Confidence, Reasoning}`. It has **no** `subtasks`, **no** `risk_level`, **no** `approval`, **no** `success_criteria`, **no** `replan`. `grep AgentPlan|subtask|risk_level` ⇒ 0 hits.
- `Planner.Plan()` (`planner.go:118-170`) is **one LLM call → one intent label**. All multi-step reasoning happens *after* it, in the ReAct loop, with tools narrowed by `IntentToolSubset`.
- There is **no AgentPlan**. `deploy_model` is a hardcoded saga (`deploy_model.go:141` `CreateInstanceDef()` — a Go-literal `[]Step`; the LLM does exactly one `matchDeployImage` choice). All 13 workflows share this shape (`workflow/registry.go:20`).

So the system today is a **deterministic workflow layer + a single-agent ReAct layer** — the first two rungs of the standard escalation ladder. For stable GPU operations, **having no AgentPlanner is the correct default, not a defect.** The naming, however, has been pulling work the wrong way: PRs #97/#128 kept stuffing *more routing* into the "classifier", because the name invites it.

**"没理清" decomposes into three scattered routing surfaces:**

| # | Surface | Where | Symptom |
|---|---------|-------|---------|
| 1 | **Naming** | the real intent-router has no package; it's buried in `engine.go:1433-1535`. The name "router" is already taken by `internal/router` (PreBlock) and `llm/router.go`. | You can't point at "the router". |
| 2 | **Prompt debt (heaviest)** | `buildSystemPrompt` (`planner.go:581-628`): 26 boundary rules, **~16 are pure lane-routing** (pricing/billing `:591`, stock/resource `:593`, deploy/lifecycle `:583`, finance 4 rules `:595-598`, route.yaml ×10 `:621-628`). | The classifier prompt has become a **natural-language routing table**. Every routing tweak = a SHA bump + 6 jitter regression tests. Every extra rule inflates per-turn context (feeds the "PriorText avalanche"). |
| 3 | **Dispatch re-judgment** | `tryPlannerDispatch` if-chain (`engine.go:1433-1535`) + `isUnsupportedHistoricalMonitorQuestion` keyword override (a **second source of truth** that can overrule the planner) + a 3-flag AND that selects the knowledge lane (`~:1516`). | **No single dispatch table owns routing.** Routing truth is split between the prompt, the if-chain, and keyword overrides. |

**~80% of the target architecture already exists** (Router=today's misnamed Planner ✅, three executors fast/terminal-RAG/agent ✅, Workflow+HITL ✅, lane-trace LR0 ✅). The work is **consolidate + rename + localize**, **not add a new layer.**

---

## 2. Target architecture (post-remediation)

**Thesis (precise — *not* "one pure table eats all routing"):** move **execution dispatch's *nominal* truth** into the pure `DispatchSpec` (bucket A); move **cross-intent tie-breakers** into `BoundaryPack`/`TieBreakerPack` (bucket B); leave the **classifier core (C)** and **output contract (D)** in the Router base prompt. The prompt becomes *Router base contract + pack projection*; the engine if-chain degrades to *runtime guards*. **No single table owns all routing** — that "one pure table" reading is the over-claim this plan explicitly rejects (§2 distinction 2).

```
User input
   │
   ▼
┌──────────────────────────────────────────────────────────────┐
│ PreBlock (internal/router)            ── UNCHANGED            │  hard safety / off-topic gate
└──────────────────────────────────────────────────────────────┘
   │
   ▼
┌──────────────────────────────────────────────────────────────┐
│ IntentRouter  (was "Planner")                                │  ONE LLM call → IntentRoute{Intent, Slots}
│   system prompt =                                            │  SEMANTIC ROUTING ONLY — emits no execution
│     bucket C  classifier core              (kept, not cut)   │
│   + bucket D  output contract              (NEVER migrated)  │  JSON shape / slots / target_refs / never-invent-UHostId
│   + bucket B  injected BoundaryPack/TieBreakerPack          │  ← PROJECTIONS of the dispatch contract
└──────────────────────────────────────────────────────────────┘
   │  IntentRoute  (pure classification — no lane, no tools, no execution)
   ▼
┌──────────────────────────────────────────────────────────────┐
│ DispatchSpec   (NOMINAL, pure, table-driven)   ── bucket A   │  intent → {NominalLane, ToolSubset, AgentSkillName}
│   the 3 pure surfaces co-located + parity-tested             │  THE routing contract: readable, deep-equal-tested
└──────────────────────────────────────────────────────────────┘
   │  SpecForIntent(intent)
   ▼
┌──────────────────────────────────────────────────────────────┐
│ ResolveDispatch(route, runtimeCtx)             ── thin guards │  applies ONLY genuine runtime state:
│   → DispatchDecision{EffectiveRuntimeForm, RouteStatus}      │   flag gates, snapshot counts, screenshot
│   (NOT a second planner — calls existing engine code)        │   suppression, per-engine enables
└──────────────────────────────────────────────────────────────┘
   │
   ├─▶ fast executor        routed tool, verbatim          (deterministic)
   ├─▶ terminal RAG         =0 rollback path only          (kept, not deleted)
   ├─▶ agent loop (ReAct)   SearchKnowledge → Diagnose* → disciplined synthesis   ◀── DEFAULT knowledge_qa (post-#251)
   └─▶ workflow / saga      mutating + HITL confirm
           └── ExecutionContract(def)  ── pure projection {name, type, tool-binding, risk?} ──▶ confirm-card / audit / (future) evaluator
```

**Three load-bearing distinctions that make this clean instead of "a second planner":**

1. **Nominal ≠ Effective.** `DispatchSpec` answers *"what is this intent's nominal shape?"* (pure, e.g. `knowledge_qa → terminal_rag`). `ResolveDispatch` answers *"given this engine's flags and this turn's runtime state, what actually runs?"* (e.g. `knowledge_qa → agent` because the agent-loop flag is on). Keeping `PlannedRuntimeFormForIntent(knowledge_qa) = terminal_rag` **pure** is what lets the table be a table; the flag-aware override lives only in `emitPlannerTrace`/`ResolveDispatch`, never in the spec.

2. **The table holds only the 3 pure surfaces.** A single `map[Intent]DispatchSpec` cannot eat the if-chain — that was the central over-claim, and it is **wrong**. Only `PlannedRuntimeFormForIntent` (`runtime_form.go:11`), `IntentToolSubset` (`tool_subset.go:6`), and `agentSkillForIntent` (`engine.go:1544`) are pure functions of intent. The runtime/per-engine guards — knowledge_qa's 3-flag gate (`~:1516`), monitor_history screenshot suppression (`:1368`), diagnosis snapshot count (`:1448`), per-engine `intentRouteIntents`/`plannerIntentEnabled` (`:1437`/`:1468`, varied across test-constructed engines) — **cannot** be tabled and **stay in `ResolveDispatch`**.

3. **Skill does not authorize tools.** Visibility (`VisibleRegistryForSubset`) and execution (`SafeToolExecutor` L2) authorize; a skill body only *requests*. The new ⊆ invariant test (PR2) nails this down.

### 2.1 Skill disclosure is contract-gated, not free discovery

The Anthropic / OpenAI Agent-Skills pattern is **progressive disclosure**: L0 `name`/`description` resident → model reads `SKILL.md` on demand → the body pulls in resources/scripts. This repo **already implements that shape** (`internal/skills/loader.go`: `SkillMeta`=L0 `:209`, lazy `Skill.Body()` `:408`, `discoverSkillResources`=L2 `:452`, `RequiredTools` `:119`).

What this product must **not** copy is **free skill discovery** — all skills' metadata resident + the model freely choosing any. We have tenant identity, write ops, billing/STS boundaries, and fixed sagas, so disclosure must be **gated by the dispatch contract**:

```
IntentRouter   → DispatchSpec      narrows candidate skills + tool subset
               → ResolveDispatch   runtime guards
               → SkillResolver     L0 metadata → L1 body → L2 resources  — WITHIN the candidate set
               → Executor          uses skill body as methodology
               → SafeToolExecutor  still the ONLY tool authorizer
```

So **`DispatchSpec` is the control plane *upstream* of Agent Skills, not a replacement.** It decides which skill metadata may be exposed this turn, which bodies may load, which tools are in scope; `SkillResolver` does Anthropic-style progressive disclosure *inside that boundary*. The invariant that keeps "skill ≠ authorization" true is `loadedSkill.RequiredTools ⊆ DispatchSpec.ToolSubset` (PR2), binding the two live surfaces `VisibleRegistryForSubset(IntentToolSubset(...))` (route, `engine.go:1139`) and `VisibleRegistryForSubset(skill.RequiredTools)` (diagnosis body, `engine.go:4086`).

**BoundaryPack ≠ Skill — separate directories.** A BoundaryPack is *router-time* (intent disambiguation / slot constraints, fed into the classifier prompt); a Skill is *execution-time* (task methodology, read by the executor). Conflating them forces the router to read skill bodies (breaking progressive disclosure) or lets the executor treat a classification boundary as methodology. Target layout:
```
internal/boundarypacks/   stock_vs_resource.md, finance_tiebreaker.md, diagnosis_vs_knowledge.md   ← router-time
internal/skills/          diagnose-ssh/SKILL.md, deploy-model/SKILL.md, ...                          ← execution-time
```

**The `DispatchSpec` struct expansion is v-next, NOT PR1.** The target shape generalizes `AgentSkillName string` → a candidate set + disclosure policy:
```go
// v-next (NOT PR1): gated on a real model-from-candidates selector existing + an eval
CandidateSkills []string
SkillDisclosure SkillDisclosurePolicy   // {ExposeMetadata, MaxBodies, MaxResources, SelectionMode}
// SelectionMode ∈ {pinned_by_intent, deterministic_by_signal, model_select_from_candidates}
```
PR1 keeps `AgentSkillName string` (§5), because that is the **only** field with a live surface to project: `agentSkillForIntent` has exactly one entry today — `{IntentDeployModel: "deploy_model"}` (`engine.go:1544`), pinned by intent. Crucially, **diagnosis is not model-select-from-candidates today**: it selects deterministically by action (`diagnosisSkillExecutorPilotForAction`, `engine.go:3999`; body loaded in `runDiagnosisSkill`, `:4073`), allowlist-gated (#125/#206). So `CandidateSkills` + `model_select_from_candidates` would encode a **selector that does not exist** (net-new behavior + its own eval) — exactly the "万能表" PR1 must avoid. The selection *mode itself* is an eval question: with flash's diagnosis under-routing (#123), `deterministic_by_signal` may stay the right default over `model_select_from_candidates`. **Prove it before building it.**

---

## 3. The organizing principle — the 4-bucket sort

Every line currently living in the planner prompt / dispatch if-chain sorts into exactly one bucket. The bucket decides where it lands after remediation:

| Bucket | What it is | Destination | Why |
|--------|-----------|-------------|-----|
| **A** | Per-intent pure facts: nominal lane, tool subset, agent skill | **Go `DispatchSpec` table** (`internal/engine/dispatch_spec.go`) | Pure functions of `intent` → table-able, deep-equal testable. |
| **B** | Cross-intent **tie-breakers** (finance: pricing vs billing vs unsupported; diagnosis-vs-knowledge; stock-vs-resource) | **`IntentBoundaryPack` / `TieBreakerPack`** → projected into the prompt | Each points at 2-3 intents at once → can't be a per-intent projection. Generalizes the *already-working* `RoutingPromptFragments()` mechanism. |
| **C** | Classifier core: how to read an utterance into an intent | **Stays in the Router base prompt** | This is the Router's actual job. Not debt. |
| **D** | Output contract: JSON shape, slot schema, `target_ref` rules, "never invent `UHostId`" | **Stays in the Router base prompt, NEVER migrated** | This is the LLM↔engine wire format. It is not routing. |

The key insight (lead's sharpest correction): **the dispatch table comes FIRST, and the prompt packs are its projection** — not the other way round. This mechanism is **already half-built**: `RoutingPromptFragments()` (`routing_registry.go:97` → `routingPromptFragmentsFrom([]RouteMetadata):105` → consumed `planner.go:621`) *already* projects `RouteMetadata` into prompt directives + examples for ~10 catalog routes. The remediation is to **generalize that proven projection to the core/legacy intents** (finance/lifecycle/diagnosis/stock that are still hand-written in `buildSystemPrompt`), thereby **eliminating the second source of truth**. Bucket B is not "do keyword pre-selection first"; it's "make the prompt a render of the contract."

---

## 4. How each component slims (整改后各组分瘦身)

### 4.1 The Router prompt (`buildSystemPrompt`, `planner.go:581-628`) — the biggest win

**Before:** 26 boundary rules, ~16 of which are a hand-maintained NL routing table. Each routing change moves the pinned SHA and trips 6 jitter tests; each rule is dead weight in every turn's context.

**After:**
- The ~16 lane-routing rules become **projections**: bucket-B tie-breakers render through the generalized `RoutingPromptFragments` mechanism; bucket-A facts don't appear in the prompt at all (they're resolved in Go).
- The base prompt collapses to **bucket C (classifier core) + bucket D (output contract)** + a small set of **injected pack fragments**.
- Net effect: the prompt **stops being a routing table**. Routing directives become a *render* of `DispatchSpec`/packs (single source of truth). Per-turn context shrinks (directly attacks the PriorText-avalanche failure mode). Maintenance localizes to one pack instead of "edit the mega-prompt + re-pin SHA + re-baseline 6 jitter tests."
- Honest caveat: per-pack injection still bumps the SHA **intentionally** when a pack changes; we switch from "one frozen blob" to "per-pack SHA + enumerable-combination assertion", with the jitter anchor refreshed by a manual PS step (not CI). The win is *localization + single-truth*, not "zero SHA churn forever."

### 4.2 The dispatch if-chain (`tryPlannerDispatch`, `engine.go:1433-1535`) — separate nominal from effective

**Before:** an if-chain that is *de facto* the routing table, interleaved with genuine runtime guards, plus a keyword override (`isUnsupportedHistoricalMonitorQuestion`) acting as a parallel truth source.

**After:**
- The **pure** decisions (nominal lane, tool subset, agent skill) are lifted out into `DispatchSpec` — a readable, deep-equal-tested table.
- The if-chain **keeps only the genuine runtime guards** (flag gates, snapshot counts, screenshot suppression, per-engine enables) and now **reads the nominal decision from `DispatchSpec`**, applying overrides via `ResolveDispatch`.
- The keyword override becomes **advisory + traced**, not a competing source of truth.
- Honest caveat: the if-chain **does not disappear** — those guards are genuinely runtime/per-engine and can't be tabled. The win is that the *table is now the readable contract* and the if-chain is *visibly just guards*, instead of routing-truth being smeared across both. Red line: **no `DispatchSpec` entry may close over runtime state** (a `HandlerKey string`/`HandlerKind enum` is fine; the knowledge_qa AND-gate closure is not — that would re-create a second planner inside the table).

### 4.3 Tool subset (`IntentToolSubset`, `tool_subset.go:6`) — co-locate + add the missing invariant

**Before:** already pure, already a function, but its relationship to the body-read skill's `RequiredTools` is only **half-enforced** (main has the route-layer `ReactToolSubsetMatchesIntentToolSubset`; there is **no** general `skill.RequiredTools ⊆ IntentToolSubset(intent)` test).

**After:**
- Becomes a **field of `DispatchSpec`** (`DispatchSpec.ToolSubset`) so the contract is co-located with lane + skill.
- PR2 adds the missing ⊆ invariant in **two parts**: a route-layer test *and* a body-read-skill test. This is **net-new** (the summary's earlier "doubly enforced" claim was an over-statement). It closes the "skill silently requests a tool the subset doesn't grant" gap and makes "skill ≠ authorization" executable.

### 4.4 Dead / drifting model-output fields (`Plan.RequiredTools`, `Retrieval.Enabled`) — derive, don't ask

**Before:** `Plan.RequiredTools` is emitted by the LLM but **ignored by the handler** (`planner.go:506`); `Retrieval.Enabled` is **always false**. They sit in the byte-pinned schema, so naively deleting them moves the SHA and churns eval.

**After:** downgraded to **derived fields** — `route.RequiredTools = DispatchSpec.RequiredToolsFor(intent)` — the same trick as "packs are projections": **turn a drifting model-output field into a deterministic projection of the dispatch contract.**
- 6a: comment-only + trace-only (do **not** touch the schema yet).
- 6b (v2): remove from the LLM output entirely, keep in trace.
- The LLM stops being asked to emit fields that are deterministically derivable — fewer tokens to get wrong, one less drift vector.

### 4.5 Naming (`Plan`→`IntentRoute`, `Planner`→`IntentRouter`) — a clarity slim, byte-neutral

**Before:** "Planner" over-claims multi-step planning the type cannot express, which is *why* people keep adding routing to it.

**After:** `Plan`→`IntentRoute` (NOT `RouteDecision` — "Decision" wrongly implies it carries execution), `Planner`→`IntentRouter`. This is a **semantics slim**: the type name stops lying about what it does. It is **byte-neutral** — only Go symbols/internal types change; the strings fed into the prompt and the trace JSON keys are **frozen** (the `registry-derived-prompt-fragment-rename-bleed` trap). Split into 2 commits: (1) Engine field + reflection whitelist in the *same* commit (zero SHA); (2) intent types, **freezing `planner.go:577`** — `"You are the IntentPlan planner …"` is the **only Go-concept name baked into the SHA-hashed prompt**, so the Go symbol and the prompt's role-string are allowed to disagree.

### 4.6 ExecutionContract (PR3) — new structure for the saga layer, not a slim of an existing one

**Before:** `workflow.Definition` (`[]Step`) is consumed ad-hoc by confirm-card, audit, etc., each reading the raw step shape.

**After:** a **pure projection** `ExecutionContract(def)` over `workflow.Definition`'s `[]Step`, giving confirm-card / audit / a future evaluator **one** structured shape with a parity test guarding drift. Because steps are **not uniform**, the per-step contract must **name the tool binding explicitly** rather than assume a static `Tool`:
```go
type ExecutionStepContract struct {
    Name        string
    Type        workflow.StepType   // StepToolCall | StepConfirm
    Tool        string              // set only when statically bound
    ToolBinding ToolBindingKind     // static | dynamic | none
    RiskKnown   bool
    Risk        security.Level      // valid only when RiskKnown
}
// StepToolCall + Tool!=""      → static,  Risk = security.Check(Tool), RiskKnown=true
// StepToolCall + ToolFunc!=nil → dynamic, RiskKnown=false (tool resolved at runtime)
// StepConfirm                  → none,    RiskKnown=false
```
This is **forced by real workflows**: `CreateInstanceDef()`'s first step 「查询镜像」 is a **dynamic** `ToolFunc` (`create_instance.go:279`, picks `DescribeCommunityImages` vs `DescribeCompShareImages` at runtime), 「确认创建」 is a **`StepConfirm`** with no tool (`:518`), and only 「创建实例」 is a static `CreateCompShareInstance`. Filling a fake `risk` for the dynamic/confirm steps would make the parity test **lie**; `RiskKnown=false` keeps it honest.
- **No `success_checks[]`** — zero existence today (`CheckResult` is an opaque closure; poll-to-Running lives handler-side). Adding them is a separate authoring pass, kept **out** of this projection PR and the Evaluator PR.

---

## 5. The PR sequence (6 PRs, dependency-ordered)

> Guiding constraint (lead, verbatim): **"第一刀必须是只读 projection + parity tests，不要试图一口气把 engine runtime dispatch、skill resolution、prompt packs、execution contract 都合并成一个'万能表'。"**

Each PR is independently `go test ./...`-green, byte-stable unless it says otherwise, and re-anchored to post-#251-merge `main`.

### PR1 — read-only DispatchSpec projection + parity (zero reroute, SHA-stable)
- **Location:** `internal/engine/dispatch_spec.go` (**NOT** `internal/intent` — `agentSkillForIntent` lives in `engine.go:1544`, and `engine` already imports `intent`; putting the spec in `intent` would force a reverse import = **import cycle, won't compile**. The feasibility verdict mislabeled `agentSkillForIntent` as a pure-`intent` function; it isn't.)
- **Add** (in `internal/engine`, intent-package types must be **qualified** — `intent.Intent`, `intent.RuntimeForm` — there is no local alias):
```go
type DispatchSpec struct {
    Intent         intent.Intent
    NominalLane    intent.RuntimeForm   // qualified — RuntimeForm is in internal/intent (runtime_form.go:3)
    ToolSubset     []string
    AgentSkillName string
}

func specForIntent(i intent.Intent) DispatchSpec {
    return DispatchSpec{
        Intent:         i,
        NominalLane:    intent.PlannedRuntimeFormForIntent(i),
        ToolSubset:     append([]string(nil), intent.IntentToolSubset(i)...), // defensive copy
        AgentSkillName: agentSkillForIntent[i],                               // map lookup; engine.go:1544
    }
}
```
  - `agentSkillForIntent` is a `map[intent.Intent]string` (not a func; `dispatch_agent_skill_test.go` already covers it), so PR1's parity test **complements** rather than duplicates it.
  - `ToolSubset` is a **defensive copy**: `IntentToolSubset` returns a fresh literal today, but the spec is a contract projection — callers must not be able to mutate the live subset.
  - `TestSpecForIntent_MatchesExistingSurfaces` — iterate **`intent.AllIntents()`** (`types.go:183`; includes legacy/mixed + deploy_model — wider than `RuntimeIntents()`), per-intent assert `NominalLane == PlannedRuntimeFormForIntent(i)`, `ToolSubset == IntentToolSubset(i)`, `AgentSkillName == agentSkillForIntent[i]`.
  - `TestSpecForIntent_ReturnsDefensiveToolSubsetCopy` — mutate the returned slice; assert `IntentToolSubset(i)` is unaffected.
- **Must stay green/unchanged:** `TestPlannerExamples_FullSystemPromptStable` still pins **`64dc6a4c…`** (the current `systemPromptSHA256Baseline` on post-#251 main — **re-read at PR time**, since any intervening prompt change moves it); `engine_session_test.go` reflection test unchanged. *(Correction: an earlier draft reversed the SHAs — `43afce19…` is the STALE value, present only in old planning docs; `#147` re-pinned to `64dc6a4c…`. Verified via `git grep` on `origin/main`.)*
- **Never touch:** `buildSystemPrompt`, `planner.go:577`, engine control-flow lines (`:1368`/`:1437`/`:1448`/`:1525`), observability JSON tags, the `Engine` struct (no new field).

### PR2 — the ⊆ invariant (route test + body-read test)
- **Framing: this PR *closes a missing* invariant — it does not merely pin an already-enforced one.** Main has only the route-layer parity (`TestRouteSkills_ReactToolSubsetMatchesIntentToolSubset`); there is **no** general body-read-skill `RequiredTools ⊆ ToolSubset` test.
- `TestRouteRequiredToolsSubsetOfRouteToolSubset` — route-manifest layer (may already pass; keep it explicit).
- `TestBodyReadSkillRequiredToolsSubsetOfAllowedToolSubset` — **net-new**, over the body-read skills in `internal/skills`. Bind it to the **actual selection path**: diagnosis bodies are chosen by `diagnosisSkillExecutorPilotForAction` + allowlist (`engine.go:3999`; #125/#206), so assert each piloted skill's `RequiredTools ⊆` the subset it actually runs under — do **not** assume a generic `CandidateSkills` selector (none exists). Makes "skill ≠ authorization" executable.

### PR3 — ExecutionContract (workflow-only projection)
- `ExecutionContract(def)` pure func + parity test, emitting `ExecutionStepContract{Name, Type, Tool, ToolBinding, RiskKnown, Risk}` (§4.6) — **explicit `static|dynamic|none` tool binding**, `RiskKnown=false` for dynamic `ToolFunc` + `StepConfirm` steps (never a fabricated risk). **No `success_checks`.** First consumer wired (confirm-card or audit); Evaluator deferred.

### PR4 — rename `Plan`→`IntentRoute`, `Planner`→`IntentRouter` (+ one-round alias)
- Keep a type alias for one release. **Freeze `planner.go:577`** and all prompt strings + trace JSON keys. 2 commits (Engine field+whitelist; intent types). Byte-neutral by construction; enumerate every SHA-delta source in the PR body (the `registry-derived-prompt-fragment-rename-bleed` lesson).

### PR5 — BoundaryPack pilot (**stock-vs-resource, NOT finance**)
- Extract the stock-vs-resource tie-breaker into an `IntentBoundaryPack` projected through the generalized `RoutingPromptFragments` path. This is the **first deliberate, contained SHA bump** — prove the per-pack-SHA + enumerable-combination assertion model on a low-stakes boundary. **Do not touch finance or diagnosis first** (finance spans pricing/billing_instance/billing_account_unsupported/knowledge_qa with multiple historical jitter fixes; pilot on the more self-contained stock boundary). Success criteria: stock/resource historical anchors **100% parity**; system-prompt SHA bumps **intentionally** with the pack SHA pinned **separately**; prompt combination is **enumerable**; **no route-status distribution drift outside the stock/resource cases**; shadow-eval the 6 jitter cases at 100% parity pre-flip.

### PR6 — derive `RequiredTools` / `Retrieval` (6a comment-only, 6b v2)
- 6a: comment-only + trace-only downgrade (schema untouched). Comment intent, verbatim: `// RequiredTools is validation/trace-only. It does not authorize dispatch and must be derived from DispatchSpec in v2.` Add a **negative test**: an LLM-emitted `RequiredTools` that mismatches must **not** change the dispatch handler / `VisibleRegistryForSubset` selection (pins "tool authorization ≠ LLM output").
- 6b: remove from LLM output, keep in trace.

---

## 6. Explicitly NOT in scope (and why)

- **AgentPlanner / PlanValidator — PARKED.** The hardcoded saga has not failed; inserting a non-deterministic decomposition step *before* mutating actions adds risk for zero gain. Trigger to revisit: step-set genuinely varies per request + combinatorial explosion + eval proving the saga drops steps (first candidate: multi-resource cluster provisioning). "Don't build it without an eval that demands it." (`docs/adr/007-framework-anti-pattern.md` principle.)
- **#128 (planner-emits-lane) — PARKED, superseded by runtime dispatch projection.** It conflicts with the "LLM emits only intent+slots, no lane" target.
- **Confirm/Acceptance → Approval rename — REJECTED.** HITL here is **self-confirmation** (`cmd/cli.go:46` `cliConfirm`; HTTP `denyConfirm`/`ConfirmBroker`; `workflow/types.go:96`). There is no independent approver; "Approval" over-claims multi-party review. `requires_acceptance` exists only in docs/ADR-003 + 1 test, **not wired**.
- **`Engine`→`AgentRuntime`, tier renames (`fast`→`deterministic_query` …) — REJECTED.** Cosmetic mega-churn that re-opens already-locked terminology; CLAUDE rule 11 (conformance > taste).
- **Evaluator + `success_checks` authoring — DEFERRED** to a separate pass after PR3's structural projection lands.
- **`DispatchSpec` struct expansion (`CandidateSkills` / `SkillDisclosurePolicy` / `SelectionMode`) — DEFERRED** to a dedicated skill-disclosure PR (§2.1), gated on a real model-from-candidates selector + an eval. PR1 keeps `AgentSkillName string`; today there is exactly one agent skill (`deploy_model`) and diagnosis selects deterministically by action, so there is nothing yet to project a candidate set from.

---

## 7. Red lines / invariants (must hold across all 6 PRs)

1. **No planner-prompt / SHA change** except PR5's *intentional, enumerated* pack bump. The pinned SHA on post-#251 main is `64dc6a4c…` (re-read at PR time; **not** `43afce19…`, which is stale).
2. **Freeze `planner.go:577`** on rename — the only Go-concept name inside the hashed prompt.
3. **No `DispatchSpec` entry closes over runtime state.** `HandlerKey string` / `HandlerKind enum` OK; the knowledge_qa AND-gate closure is **forbidden** in the table (→ `ResolveDispatch`).
4. **Nominal stays pure.** `PlannedRuntimeFormForIntent(knowledge_qa) = terminal_rag`; the agent-loop override lives only in trace/resolve, never in the spec.
5. **Bucket D (output contract) is never migrated** out of the base prompt.
6. **Byte-stability is asserted, not assumed** — every PR that *could* move the SHA enumerates the delta sources in its body and refreshes the jitter anchor by hand.
7. **Re-verify line numbers against post-#251-merge `main`** before writing each PR.

---

## 8. Sequencing relative to #251

PR #251 (collapse `knowledge_qa` RAG into the agent loop + disciplined synthesis, default-ON) is the upstream merge. This restructure **anchors to post-#251-merge `main`**. Until #251 merges (user self-manages merges), only the **ADR (009)** and this plan doc are authored; PR1 starts once `origin/main` contains #251.

**Value/risk profile (feasibility verdict):** the high-value/low-risk core ≈ **60% of the value at ~20% of the risk** = PR1 (narrowing/projection) + PR2 (⊆ test) + PR3 (contract projection) + PR4 (safe rename). PR5/PR6 are the contained-SHA-bump tail.
