# B2b — Skill/Tool Directory Split + Codegen + Progressive Disclosure

**Date**: 2026-05-29
**Driver**: ADR-003 (skill ⊥ tool) + ADR-004 (skill bundle + progressive disclosure) + ADR-003 Amendment-1 (MCP naming/visibility). Expands the B2b *outline* in `docs/plans/2026-05-29-b2-router-inject-completion.md:165-198`.
**Audience**: implementer (Codex/Claude) + reviewer
**Estimated effort**: 2-3 weeks, phased (P1 ~3d, P2 ~4d, P-tool ~4d, P3 ~3d + validation)
**Status**: Draft **rev-3** (2026-05-29; rev-1 adversarial review applied — 2 blockers + 8 majors fixed; rev-3 folds cross-artifact review F1-F4; see §A)
**Depends on**: B2a shipped (`b38fe28`, on main). **Blocks**: B4b planner-tier (needs the ≤3k prompt this delivers).

> Grounded in a full current-state map (workflow `wf_611cd891`) + a 5-lens adversarial review (`wf_4bfd92ca`). Every "current" claim carries file:line, re-verified in review.

---

## A. rev-2 changelog (what the review fixed)

- **B-1 (blocker):** P-tool generator must mirror **`policyForAction(name)`** (`policies.go:93`), NOT `ActionLevels[name]` — Registry tool names are `*Workflow` wrappers absent from `ActionLevels` (`StopInstanceWorkflow` `registry.go:417` vs `StopCompShareInstance:L1` `levels.go:71`); `ActionLevels[name]` misses on all 11 Workflow tools + the 2 knowledge tools and falls to `inferredRegistrySecurityLevel` (`policies.go:184`). Fixed in §6.
- **B-2 (blocker):** No L2 action is a Registry/LLM tool (`Terminate*`/`Delete*` absent from `registry.go`; `levels.go:94,96`). L2 always-refuse **stays in `security/levels.go` + `safe_executor.go:189`**, out of the JSON mirror. `destructive` is a future-MCP-projection field, NOT a B2b runtime gate. Fixed in §6/§11.
- **Flag split:** `USE_SKILL_REGISTRY` (P2, byte-identical source swap) vs `USE_PROGRESSIVE_DISCLOSURE` (P3, prompt-shape change) — rev-1 overloaded one flag with two regimes. §1/§5/§7/§8.
- **Plan.Skills + body-on-trigger is agent-tier, OUT of B2b:** the 6 capabilities are fast-tier deterministic handlers (no LLM body injection); B2b's P3 is **only** the metadata-only planner-prompt shrink. §7/§9/§12.
- **≤3k is a measured metric, not a 0-violation gate** (token count is statistical-family, memory `hard-contractual-gates-binary`); the binary gates are dispatch/handler byte-identity + fast/knowledge-never-`Body()`. §9/§11.
- **Dropped the `safety_warning`/`deploy_model` cross-tier acceptance bullet** — neither skill exists, `deploy_model` is agent-tier (not built in B2b); deferred to the agent-tier batch. §9.
- **x-compshare key set corrected** to the full Amendment-1 set + sources per key. §6.
- Plus minors: §5 test-label correction, ADR-003 also-Proposed precondition, P1 binary gate, eval-target anchor, count-test-impact, HTTP-path subsection, citation fixes.
- **rev-3 (cross-artifact review):** **F1** — reconcile ≤3k vs the roadmap's "B2b unblocks B4b": unconditional for B4b's *schema change*, but the planner *flash* cost-down (open decision #1) may need a directive-tiering follow-up if the `base`-block residual keeps the prompt above a flash-safe size (§12.4 + roadmap). **F2** — the 6 capabilities' bodies are vestigial in B2b (forward-compat only). **F3** — required (`verification_status`/`field_refs_verified`) vs optional (routing block) field split made explicit for `KnownFields(true)`. **F4** — codegen must emit deterministic ordering or the digest pin + byte-identity gate go flaky.

---

## 0. Preconditions (binary — resolve before P1)

1. **Ratify ADR-003 + ADR-004 (both `Proposed`** — `003:3`, `004:3`). P1's loader hard-codes ADR-004's frontmatter keys + caution semantics (`004:81,104`) and parses with `KnownFields(true)` (`capability_registry.go:241`), so any later schema-key change is a hard-fail rewrite. P-tool builds JSON to ADR-003's enum and proposes amending it (B-2). **Gate is binary** (Rule 12): P1 MUST NOT start until ADR-003/004 + the §3 superset schema (open decisions #1/#2) are ratified. If the lead chooses to start before ratification, record it as an explicit deviation in the PR description — not an inline "or accept risk" hatch.
2. **Tool-binding audit (resolved 2026-05-29):** four binding sources exist, and they are not all "drift":
   - (1) `capabilityRegistry` Go literal (`capability_registry.go:40-47`, runtime dispatch source-of-truth)
   - (2) frontmatter `required_tool` (singular), reconciled to (1) by init panic (`:144-164`)
   - (3) `extraHandlerActions()` (`:49-56`, stock's 2 extra gated tools)
   - (4) `IntentToolSubset()` (`internal/intent/tool_subset.go:6`, the ReAct-fallback-visible set)
   - **(2) and (4) encode different concepts** — the cutover handler's single deterministic tool vs the broader set the LLM sees on ReAct fallback. `gpu_specs` has `required_tool: DescribeAvailableCompShareInstanceTypes` but `IntentToolSubset = {DescribeAvailableCompShareInstanceTypes, GetGPUSpecs}`, and **`GetGPUSpecs` is a real registered tool** (`registry.go:11`), not phantom. Codegen **cannot** derive `tool_subset` from `required_tools[]`; both stay declared data. → §3 carries `react_tool_subset` as a distinct field.

---

## 1. Why phased (P1 → P2 → P-tool → P3)

Single-variable phases so a planner regression is bisectable (memory `phase-a-strict-zero-behavior`, `smoke-test-cannot-infer-architectural-decision`). **Two independent flags, two independent cutover cycles.**

| Phase | What | Flag | Behavior change |
|---|---|---|---|
| **P1** | `loader.go` + `cmd/skillgen` + caution injection + CI, against the 5 seeded `diagnose_*` skills | — | none (no routable consumer) |
| **P2** | Migrate the 6 capabilities to `internal/skills/<name>/skill.md`; codegen `skillRegistry` becomes the dispatch + eager-prompt source | `USE_SKILL_REGISTRY` (default off) | **none** (flag-on byte-identical to legacy) |
| **P-tool** | `internal/tools/<tool>.json` + `x-compshare`, generator = validated mirror of `policyForAction` (parallel, disjoint surface) | — | none (generated `Registry` byte-rebuilds the slice) |
| **P3** | Planner prompt: eager directives+examples → metadata-only catalog | `USE_PROGRESSIVE_DISCLOSURE` (default off) | **yes** (planner prompt bytes change; only this phase) |

P1/P2/P-tool are zero-behavior-change (the B2a discipline). Only P3 changes what the planner sees.

---

## 2. Current state (verified, file:line — passed factual-accuracy review)

- **6 active capabilities**, all `skill_group: catalog`: `gpu_specs_query`, `stock_availability`, `platform_image_list`, `custom_image_list`, `community_image_list`, `pricing_query` (`internal/intent/capabilities/*.md`; `_general_tech_qa.md.disabled` excluded via `_`-prefix skip `capability_registry.go:179`).
- **Two incompatible frontmatter dialects:**
  - *Capability* (`CapabilityMetadata`, `capability_registry.go:107-122`): `name, intent_label, skill_group, required_tool (singular), tool_subset, required_citation, planner_directives[], planner_examples[{question,confidence}]`.
  - *ADR-004 skill* (on disk, `diagnose_ssh/skill.md:1-19`): `name, description, triggers[], applicable_tiers[], required_tools[] (plural), related_skills[], body_cap_lines, verification_status, field_refs_verified`.
  - The 6 capabilities lack `description/triggers/verification_status`; the skill dialect lacks `planner_directives/planner_examples` (the Stage-2C routing anchors). Naive mapping onto bare ADR-004 **drops routing signal** → silent planner regression.
- **`internal/skills/` has 5 seeded `diagnose_*` skills** (commit `b884312`), ADR-004 dialect, **all `verification_status: unverified` + `field_refs_verified: false`**, **no `loader.go`/`cmd/skillgen`/`registry_gen.go`/flag** (typed grep clean). Note: `diagnose_ssh/skill.md:15` has a **dangling `related_skills: [safety_warning]`** pointing at a skill that does not exist — the loader must tolerate (or explicitly flag) dangling `related_skills` refs.
- **Planner prompt** (`buildSystemPrompt()`, `planner.go:520-527`): `base` (a ~37-line `strings.Join` literal `planner.go:476-514`, ~30 of which are classifier directives, the rest schema/enum scaffold) + `renderPlannerPromptExampleGroups(...)` + capability `directives` + capability `examples`. All eager. `CapabilityPromptFragments()` (`capability_registry.go:251-303`) injects full directives + one synthesized Plan-JSON per example (**not** the body). Measured: **24,735 chars / 175 lines**; example groups dominate (12,335 chars / 113 lines). ~6k tokens.
- **The `base` block is global classifier rules, not skill metadata** → progressive disclosure shrinks the capability/example segment, not `base`. After examples move out, `base` is the dominant residual → **≤3k may need a separate directive-tiering effort** (out of B2b; §12).
- **Gates that fire on prompt drift:** `systemPromptSHA256Baseline=1f6823...` + `TestPlannerExamples_FullSystemPromptStable` (`planner_examples_test.go:200-213`); count assert `len(examples)==28+capabilityExampleCount` (`planner_prompt_test.go:79`); grouping assert total **47** with exact per-intent counts (`planner_prompt_test.go:102-214`); enum-substring + Routes* asserts (`planner_prompt_test.go:21-25`); byte-equal disk-loader-equals-legacy (`planner_examples_test.go:62-69,391-398`); YAML `KnownFields(true)` (`capability_registry.go:241`). The **registry↔frontmatter** parity net specifically is the init panic (`capability_registry.go:144-164`) + `TestCapabilityPromptFragments_DeriveFromMarkdownFrontmatter` (`capability_registry_test.go:116`) — distinct from the registry↔whitelist/tool/subset asserts (`capability_registry_test.go:64-101`, `tool_subset_test.go:54-74`).
- **Tool layer**: 31 tools as Go literals in `var Registry []openai.Tool` (`registry.go:6`). Subset mapping is `IntentToolSubset()` in `internal/intent/tool_subset.go`. **Security level + class are runtime-composed by `policyForAction(action)` (`policies.go:93`)**: `ActionLevels[action]` (`security/levels.go:28`) with fallback `inferredRegistrySecurityLevel` (`policies.go:184`, `Workflow`-suffix→L1); `class=classForAction` (`:193`); `NeedsConfirm = (level==L1)` (`:104`). **`ActionLevels` keys are API-action names; Registry tool names are `*Workflow` wrappers** — they do not coincide. L2 actions (`TerminateCompShareInstance` `levels.go:94`, `DeleteCompshareDisk` `:96`, +2) are **deliberately not registered as LLM tools** (grep `registry.go` clean). Two gates: visibility (`VisibleRegistry` `registry.go:801`, hides Workflow/mutating unless enabled; route from `routeForAction` `policies.go:171`) + execution (`safe_executor.go:189-194`, always-refuse L2/destructive, refuse mutating-when-off). `COMPSHARE_ENABLE_MUTATING_TOOLS` parsed only at cmd layer (`trace.go:159-169`).
- **Handlers are imperative Go** (`handleGPUSpecsQuery`/renderers/envelope builders, `capability_registry.go:338-1069`) — codegen can't eliminate; the `handler` field is a func ptr (`:37`).
- **`Plan` struct (`types.go:95-105`) has NO `Skills` field** (verified: grep clean); any `skills[]` is greenfield. `parsePlanJSON` uses plain `json.Unmarshal` (`planner.go:203`, no `DisallowUnknownFields`) → the LLM can emit unknown keys without a parse error.
- **Engine dispatch is already generic** (`IsCapabilityIntent`→`DispatchCapability`, `capability_registry.go:61-99`; one hook at `engine.go:961`). The engine has **cutover + ReAct only — no fast/knowledge/agent tier router yet** (that's B4b/agent work).

---

## 3. Unified skill frontmatter schema (proposed — open decision #1)

Superset both dialects so routing anchors survive and one loader reads everything. **This schema requires an ADR-004 amendment** (ADR-004's schema `004:36-53` has no extensibility clause); it is the design P1/P2 build against once ratified (Precondition 1).

```yaml
# internal/skills/<name>/skill.md
---
name: gpu_specs_query              # snake_case [a-z][a-z0-9_]*[a-z0-9], 1-64; dir name MUST equal this
description: "..."                  # one line, planner-visible
triggers: ["...", "..."]           # classification anchors, advisory-only
applicable_tiers: [fast]
required_tools: [DescribeAvailableCompShareInstanceTypes]  # plural; absorbs the singular required_tool
react_tool_subset: [DescribeAvailableCompShareInstanceTypes, GetGPUSpecs]  # NEW; ReAct-fallback set (≠ required_tools — Precondition 2)
related_skills: []                 # may dangle (e.g. safety_warning not yet authored) — loader tolerates
body_cap_lines: 100
verification_status: production_validated   # the 6 migrated are live + field-checked
field_refs_verified: true
# routing block (carried from capability dialect; widens ADR-004 — open decision #1):
intent_label: gpu_specs_query
skill_group: catalog
required_citation: false
handler_key: handleGPUSpecsQuery   # name→func map entry, existence-checked at generate time
planner_directives: ["..."]
planner_examples: [{question: "4090 显存多大", confidence: 0.85}]
---
<markdown body — 正例/反例/边界, ≤100 lines (frontmatter excluded from the count)>
```

All 6 dir names are already snake_case → zero rename churn. Codegen emits `IntentToolSubset` from `react_tool_subset`; `extraHandlerActions()` folds in as `required_tools[1:]`.

---

## 4. Phase P1 — loader + codegen + caution + CI (zero behavior change)

Build the machinery against the 5 seeded `diagnose_*` skills (natural unverified-path fixtures; no routable consumer → no behavior change).

- `internal/skills/loader.go` (~300 lines, `004:108-137`): `NewLoader(root)` scans `root/*/skill.md`, parses **frontmatter only** (reuse the `---` splitter `capability_registry.go:225-247`), body unread. `Skill.Body()` (`sync.Once`) reads, strips frontmatter, enforces `body_cap_lines` (default 100, ceiling 200, **build/load fail not truncate**), and — single choke point — prepends `[CAUTION: this methodology is unverified...]` when `verification_status==unverified` and appends `[FIELD REFS NOT VERIFIED...]` when `field_refs_verified==false` (`004:81,104`). `Metadata()` projects name+description+triggers (~80 tok). `Fetch(name)`.
- **Strict parse** (CLAUDE.md convention): missing/empty/unknown `verification_status`/`field_refs_verified` → hard-fail at `NewLoader`, never default permissive (`004:88`). Dangling `related_skills` (e.g. `safety_warning`) → warn, do not hard-fail (it's a forward reference). **(F3) Required vs optional:** ONLY `verification_status` + `field_refs_verified` are hard-required; the §3 routing block (`planner_directives`, `planner_examples`, `handler_key`, `intent_label`, `skill_group`, `required_citation`) is OPTIONAL, so the 5 `diagnose_*` skills (which omit it) still parse. `KnownFields(true)` rejects *unknown* YAML keys, not *missing* optional struct fields — so the union struct must contain every key from both dialects, and validation (not parsing) enforces the required subset.
- `cmd/skillgen` (~150 lines, `004:170`): `go:generate go run ./cmd/skillgen ...` → `registry_gen.go`. **Generate-time existence check** that every `handler_key` resolves in a hand-maintained `handlerByName` map (mirrors `capability_registry.go:144-164`). **(F4) Deterministic output:** skillgen MUST emit `registry_gen.go` with stable ordering (sorted skill names, stable example order) — Go map iteration is randomized, so non-deterministic codegen would make the digest pin (below) AND the P2 byte-identity gate (§5) flaky. Iterate over a sorted key slice, never a bare `range map`.
- **Digest pin** (stronger than `004:179`'s "git status clean"): pin LF-normalized `sha256` of `registry_gen.go` analogous to `internal/knowledge/corpus_digest.go`; CI runs `go generate && git diff --exit-code` + digest verify. LF-normalize before hashing (CRLF on Windows).
- CI: `scripts/check_skill_names.sh` + `scripts/check_skill_caps.sh` into `.githooks/pre-commit` (`.ps1` variants — hook requires PowerShell).
- **Tests**: loader unit (lazy body, frontmatter-skip count, name==dir, dual caution injection), **`sync.Once` concurrent-`Fetch` `-race`** (add `internal/skills` to a `-race` target — only `internal/entity` is raced today; `sync.Once` is the named risk), body-cap build-fail fixture, codegen drift.

P1 acceptance: `go test ./...` green, 5 skills load with both caution lines, **engine unchanged** (no consumer).

---

## 5. Phase P2 — migrate the 6 capabilities (zero routing change, `USE_SKILL_REGISTRY` off)

- Move each capability to `internal/skills/<name>/skill.md` in §3 schema; body = existing markdown (`pricing_query.md` is the longest at ~36 body lines / ~52 whole-file, well under the 100-line body cap). **(F2) The body is vestigial in B2b for these 6 fast-tier capabilities** — handlers are deterministic so the body is never injected into an LLM prompt (§9 asserts fast/knowledge never call `Body()`), and today the capability markdown body is already unused (`CapabilityMetadata.Body` is `yaml:"-"`; `CapabilityPromptFragments` injects directives+examples, not body). `body_cap` on them is forward-compat only (for if a capability later gains an agent-tier playbook), not a live constraint.
- `USE_SKILL_REGISTRY` (**default off**, env/config-only, unknown→off+warn). Behind it, the generated `skillRegistry` is the dispatch + eager-prompt source; `legacyCapabilityRegistry` stays alive. `IsCapabilityIntent`/`DispatchCapability` and `CapabilityPromptFragments()` read whichever the flag selects.
- **Flag-off invariant (phase-a-strict-zero-behavior):** flag-off = legacy path byte-for-byte; `systemPromptSHA256Baseline` stays pinned to legacy and `TestPlannerExamples_FullSystemPromptStable` stays green **untouched**. **Flag-on is also byte-identical** (eager generated registry produces the same prompt + same dispatch). Add a test asserting flag-on == flag-off byte-for-byte.
- Re-point the parity nets at the generated registry, keep green through cutover (re-point, don't rewrite): registry↔whitelist/tool/subset asserts `TestHandlerActionWhitelist_DerivesFromRegistry` (`capability_registry_test.go:89-101`), `TestCapabilityRegistry_BindsToRealTool` (`:64-82`), `TestIntentToolSubset_CapabilityIntents` (`tool_subset_test.go:54-74`, counts `{gpu_specs:2,stock:3,pricing:2,image-lists:1}`); **and** the registry↔frontmatter net (init panic `capability_registry.go:144-164` + `TestCapabilityPromptFragments_DeriveFromMarkdownFrontmatter` `:116`).

P2 cutover (this is a **byte-identity** dual-run — distinct from P3's): ship flag-off, two-pass diff (off=legacy vs on=skillRegistry), deterministic dispatch + handler output + prompt **byte-identical** (0-violation gate, memory `hard-contractual-gates-binary`), flip `USE_SKILL_REGISTRY=on`, keep legacy 2 weeks, delete in follow-up.

---

## 6. Phase P-tool — tool JSON + x-compshare (parallel, zero behavior change)

Disjoint surface from P2. One `internal/tools/<tool_name>.json` per **Registry tool** (the L0/L1 tools that are LLM-visible) = OpenAI `FunctionDefinition` + `x-compshare`. `go:generate` rebuilds `var Registry []openai.Tool` byte-for-byte so `engine.go:961` + `policies.go:223` (`registryAllowedParams`/`filterSafeArgs`) callers are unchanged.

- **`x-compshare` is a generated/validated MIRROR of `policyForAction(name)` (B-1), not `ActionLevels[name]`** — `policyForAction` is the actual single runtime producer and already composes the `ActionLevels` lookup + the `Workflow`-suffix fallback (`policies.go:93-97`) + `classForAction` + `NeedsConfirm`. Generator asserts per tool and **build-fails on mismatch**:
  - `x-compshare.class` == map(`policyForAction(name).Class`)  *(ActionLevels-mirrored)*
  - `x-compshare.requires_acceptance` == `policyForAction(name).NeedsConfirm` (i.e. `level==L1`)  *(mirrored)*
- **Full `x-compshare` key set** (Amendment-1 `003:186-192` = `class, requires_acceptance, idempotent, tier_eligible, api_action`; **+ `destructive`** proposed):

  | key | source | validated? |
  |---|---|---|
  | `class` | `policyForAction.Class` | yes (equality-assert) |
  | `requires_acceptance` | `policyForAction.NeedsConfirm` | yes (equality-assert) |
  | `destructive` | (Amendment-2) future-MCP mirror — see below | no runtime use in B2b |
  | `idempotent` | hand-authored | reviewer-checklist only (no current source) |
  | `tier_eligible` | hand-authored, **default-off/advisory** | not a router in B2b |
  | `api_action` | the tool's API action name (1:1 / mapped) | presence-checked |

  Generator build-fails if any of `{class, requires_acceptance, api_action}` is omitted or mismatched; `{idempotent, tier_eligible, destructive}` are presence-checked (Amendment-1 review-checklist, `003:207`) but hand-authored.
- **L2 always-refuse stays OUT of the JSON mirror (B-2):** no L2 action is an LLM/Registry tool, so the JSON surface (the 31 exposed tools) carries no L2. The always-refuse for `Terminate*`/`Delete*` remains solely in `security/levels.go` + `safe_executor.go:189`. `x-compshare.destructive` is a **faithful mirror for spec-completeness + future MCP projection**, NOT a B2b runtime gate input (authority stays in `security.ActionLevels`). For the exposed set, the L2 mapping is moot; if `destructive` is ever populated, set `{class:mutating, requires_acceptance:false, destructive:true}` — `requires_acceptance:false` because always-refused L2 has no HITL/confirm path (`safe_executor.go:189`, `NeedsConfirm` only at L1 `policies.go:104`), which is exactly what the equality-assert against `NeedsConfirm` produces.
- **`tier_eligible` ships default-off/advisory** (memory `planner-hint-advisory-only`, `new-protocol-frame-default-off`); `IntentToolSubset` stays the live routing source. **Do not build `internal/mcp/naming.go`/`projection.go`** — B7 (`003:174,194`). B2b only ensures `x-compshare` field names match the Amendment-1 visibility table.

---

## 7. Phase P3 — progressive disclosure (the only behavior change, `USE_PROGRESSIVE_DISCLOSURE` off)

**Scope is the planner-prompt shrink only.** The 6 capabilities are fast-tier **deterministic handlers** — their bodies are never injected into an LLM prompt — so B2b needs no `Plan.Skills` emission and no agent-tier body-injection point (which doesn't exist yet). Those are **agent-tier deliverables, out of B2b** (§12).

- Behind `USE_PROGRESSIVE_DISCLOSURE`: replace the eager capability section of the planner prompt (`CapabilityPromptFragments()` → directives + synthesized example JSON) with `renderSkillCatalog(loader.Metadata())` — one `name — description | triggers` line per **routable** skill. Dispatch stays **intent-based** (unchanged).
- **The real risk = classification-signal loss.** The removed `planner_directives`/`planner_examples` are the anchors the Routes* + grouping tests pin (`planner_prompt_test.go:21-25,102-214`); replacing them with one-line metadata may regress planner classification of the 6 intents. This is the empirical bet ADR-004 makes — validate, don't assume (§10, memory `eval-target-must-match-runtime-path`).
- **Excluded from the catalog:** `unverified`/`spike`-only skills (the 5 `diagnose_*`) — so the metadata reflects only production-routable skills (memory `constraints-anchor-to-validated-artifact`) and unverified methodology never enters the prompt.
- **Tests that break at P3 (not just the SHA):** `systemPromptSHA256Baseline` re-pin **and** the count assert (`planner_prompt_test.go:79`, `28+capabilityExampleCount`) **and** the grouping assert (`:102-214`, total 47) must be rewritten — the metadata-only catalog changes the example set those tests count. Add a **second** baseline constant (`systemPromptSHA256BaselineProgressive`) so both paths are pinned during the dual-run window; re-pin the primary only at flip.

---

## 8. P3 cutover + double-run (resolves the rev-1 contradiction)

P2 already byte-validated the source swap (§5). **P3's dual-run validates the prompt-shape change**, which by §7 is *not* byte-identical — so it cannot use a pure byte-identity gate. Two-pass diff, not per-turn shadow (memory `attribution-observable-only`):

1. Ship P3 code behind `USE_PROGRESSIVE_DISCLOSURE=off` (planner prompt unchanged = legacy eager).
2. Run the CLI/HTTP smoke suite **twice** — pass A (off, eager prompt) vs pass B (on, metadata-only prompt) — diff the trace sets. Trace records **only** the path that served each turn.
3. **Split the gate:**
   - **Deterministic (hard, 0-violation):** for every turn where the planner classified the **same intent** in both passes, the cutover dispatch target + handler output + selected `required_tools` are **byte-identical** (handlers run without LLM). Compare via a new dispatch/handler-output deep-equal harness (the planner-example byte-equal pattern at `planner_examples_test.go:45-80` is a *prompt-string* surface, not handler output — a new harness is needed).
   - **Statistical (reported, not gated at 100%):** planner intent-classification **agreement** across passes, N=20, reported as % (LLM jitter, memory `jitter-check-for-classification`). A drop here = the classification-signal-loss risk materialized → do not flip.
4. Flip `USE_PROGRESSIVE_DISCLOSURE=on`; keep eager path 2 weeks. **Rollback trigger:** any deterministic-gate violation in prod traces, or a classification-agreement drop below an agreed threshold, flips the flag off (config-only **for the CLI**; see HTTP caveat §8a). Delete the eager path in a follow-up.

### 8a. HTTP/server path (rev-2 addition)

- The planner lives in process-shared `SharedDeps.IntentPlanner` (`engine.go:258`; CLI sets via `SetIntentPlanner` `cmd/cli.go:114`, server via `deps.IntentPlanner` `cmd/shared_deps.go:86`). The loader/registry is process-global (built at boot). **Per ADR-004:200 there is no hot reload** (`sync.Once` + restart). So flipping either flag for the **server** requires a **restart**, not "config-only no redeploy" — reconcile the rollback story accordingly (CLI re-reads env per run; server re-reads at boot).
- Read points: `cmd/trace.go` (CLI), `cmd/shared_deps.go` (server). The loader is constructed **once** and shared via `SharedDeps` across the agentpool's per-session engines, not per session.
- The HTTP path uses `denyConfirm` (`agentpool/rehydrate.go:15`) — it can never confirm L1 — so the deterministic two-pass gate on the HTTP path must restrict to read/no-confirm turns or run the mutating turns on the CLI path.

---

## 9. Acceptance (merged ADR-003 + ADR-004 + Amendment-1)

- [ ] `internal/skills/` holds all **6 active capabilities** as `<name>/skill.md`, snake_case `name`==dir (`004:218`).
- [ ] Every `skill.md` (incl. the 5 `diagnose_*`) carries `verification_status` + `field_refs_verified`, no defaults, CI-existence-checked (`004:88`).
- [ ] `loader.go`: lazy `Body()` + `sync.Once` + unit tests incl. concurrent-`Fetch` `-race` (`004:219`).
- [ ] `cmd/skillgen` produces `registry_gen.go`; `go generate ./...` clean-tree + digest-pin check in CI (`004:220`).
- [ ] Progressive disclosure: planner prompt = `Metadata()` only (routable skills); dispatch stays intent-based; **test asserts fast/knowledge never call `Body()`** (`003:91`, `004:144`).
- [ ] `internal/tools/<tool>.json` for each Registry tool with the full `x-compshare` key set; generator equality-asserts `{class, requires_acceptance}` against `policyForAction` and presence-checks the rest (`003:89,207`).
- [ ] `scripts/check_skill_caps.sh` (frontmatter-skipping, fail >200) + `scripts/check_skill_names.sh` in `.githooks/pre-commit` (`004:221,69`).
- [ ] **Metric (reported, not a 0-violation gate):** N=20 sampling records the metadata-only planner-prompt token count; **target ≤3k**, flag if exceeded. *If the `base`-block residual keeps it >3k, file directive-tiering as a follow-up and report the floor (open decision #4) — the binary gates are §8.3's dispatch/handler byte-identity + fast/knowledge-never-`Body()`, not the token count.*

**Deferred out of B2b** (was rev-1 acceptance, removed): the `safety_warning`/`deploy_model` cross-tier-reuse bullet (`004:222`) — `deploy_model` is agent-tier (not built in B2b) and neither skill exists. Carried to the agent-tier batch. `Plan.Skills` emission + body-on-trigger budget (`004:159-165`) — agent-tier infra; the 6 capabilities don't need it.

---

## 10. Test plan (memory `cli-regression-for-capability-changes`)

1. `go test ./... -count=1` green incl. SHA + `planner_examples` + leak-audit (unchanged through P1/P2/P-tool).
2. Loader unit + `-race`; body-cap build-fail fixture; frontmatter-skip count; name==dir; dual caution injection; dangling-`related_skills` warn-not-fail.
3. Codegen drift (`go generate && git diff --exit-code`) + digest pin.
4. P-tool: generator equality-assert against `policyForAction` for all 31 tools (incl. the 11 `*Workflow` + 2 knowledge tools — the B-1 cases); `registryAllowedParams` parity (`policies.go:221-230`).
5. **Two-pass CLI smoke** over all 6 capability intents + `IntentDiskInfo` (planner-only) + ≥1 zero-target `operation_lifecycle`. **Mandatory** — `go test` is insufficient for capability changes.
6. P3 only: N≥5 same-question jitter **through the real planner** with the metadata-only prompt, reading `trace.planner.intent` (memory `eval-target-must-match-runtime-path`, `routing-verification-via-trace-not-latency`, `jitter-check-for-classification`) — the deterministic gate cannot catch classification drift from removing the pinned directives. N=20 ≤3k measurement on the **production-routable** set only.
7. HTTP smoke: P2/P-tool = **byte-identical parity** (B2a 4-turn matrix, `eval/traces_server_local`), flag-off vs flag-on; P3 = the §8.3 split (deterministic dispatch + statistical classification), **not** end-to-end parity (the prompt differs by design).

---

## 11. Risks

- **Classification-signal loss (P3, headline):** removing `planner_directives`/`planner_examples` may regress the planner's routing of the 6 intents (the Routes*/grouping tests pin them). Mitigation: §3 superset keeps them available; §10.6 validates through the real planner; flag stays off until N=20 agreement holds.
- **SHA + count + grouping tests** all fire at P3 (not just SHA). Mitigation: rewrite all three; dual baseline; flag-off keeps legacy pinned.
- **Routing-signal mapping** (Precondition 2): `react_tool_subset` must stay declared data (can't derive from `required_tools`).
- **Unverified bodies**: the 5 `diagnose_*` are `unverified`+`field_refs_verified:false` citing concrete API fields. Mitigation: excluded from routable catalog + dual caution lines (single choke point).
- **Mutation-gate drift (P-tool):** hand-authored `x-compshare` could flip a gate. Mitigation: generator equality-assert against **`policyForAction`** (B-1), build-fail on mismatch; preserve `routeForAction` (`policies.go:171`) suffix derivation (else `*Workflow` tools leak into read-only mode via `VisibleRegistry` `registry.go:812`).
- **L2 not in the JSON surface (B-2):** L2 always-refuse stays in `security/levels.go` + `safe_executor.go:189`; `destructive` is mirror-only — **no B2b runtime behavior depends on it**.
- **`sync.Once`** unraced unless `internal/skills` joins a `-race` target.
- **`KnownFields(true)`**: any new YAML key hard-fails parsing unless the struct is extended in the same change (sequence P1 struct before P2 migration).
- **HTTP no-hot-reload**: flag flips need a server restart (§8a) — the rollback story is config-only for CLI, restart for server.
- **≤3k may be unreachable from metadata alone** (`base` residual): reported as a metric, not a blocking gate (§9).

---

## 12. Open decisions — **RESOLVED 2026-05-30 (gate CLOSED)**

> Lead sign-off 2026-05-30. ADR-003/004 **provisionally accepted** (revert only if a
> measured regression appears — lead's read: unlikely, current system too messy).
> B2b may enter implementation (P1 loader+codegen); lead deferred the *start* to
> post-compact. Items below record the resolution.

1. **Schema widen + ratify** — ✅ **YES (lead-confirmed).** Widen ADR-004 frontmatter
   with the §3 routing block (`planner_directives`, `planner_examples`, `handler_key`,
   `react_tool_subset`, `intent_label`, `skill_group`, `required_citation`).
   `react_tool_subset` stays **declared data, distinct from `required_tools`**
   (Precondition 2 — must not be derived/conflated). `destructive`: **Amendment-2,
   future-MCP mirror ONLY — no B2b runtime gate** (L2 always-refuse stays in
   `security/levels.go` + `safe_executor.go:189`); defer the field's *consumer* to B7.
2. **`idempotent` source** — ✅ **reviewer-checklist-only (recommended default; lead may veto).**
   Drop it from the build-fail-on-omit set; defer a generator-validated `idempotent`
   table to B7 (same "future-MCP field" basis as `destructive`). Keeps B2b codegen
   scoped to the security-level mirror.
3. **`safety_warning` / `Plan.Skills` / body-budget** — ✅ **DEFERRED to agent-tier
   (B6–B8) (confirmed).** The 6 capabilities are fast-tier deterministic handlers; no
   LLM body injection exists yet, so these are out of B2b by construction (§7/§9).
4. **≤3k token** — ✅ **reported metric, NOT a gate (lead-confirmed).** Lead relaxed
   the token-cost constraint (cheap model). Decision #1 settled the planner to **flash**
   and showed the avalanche/jitter is prompt **content/structure**, not size — so don't
   byte-shave to 3k. Progressive disclosure ships for clarity/maintainability; the
   reliability lever is the prompt-structure work (imperative directives + `target_ref`
   few-shot, tracked as a standalone planner-prompt PR). `directive-tiering` is a
   *follow-up if ever needed*, never a B2b blocker. (Reconciled in `roadmap.md`.)

---

## 13. Refs

- ADR-003 (skill ⊥ tool + Amendment-1), ADR-004 (bundle + progressive disclosure), ADR-005 (the 5 diagnose skills' provenance)
- B2a spec `docs/plans/2026-05-29-b2-router-inject-completion.md` §B2b (outline this expands)
- Roadmap `docs/plans/roadmap.md` (B2b = critical path)
- Groundwork `wf_611cd891`; adversarial review `wf_4bfd92ca` (5 lenses; rev-2 applied)
- Memory: `phase-a-strict-zero-behavior`, `new-protocol-frame-default-off`, `hard-contractual-gates-binary`, `attribution-observable-only`, `jitter-check-for-classification`, `cli-regression-for-capability-changes`, `priortext-avalanche-invalidates-planner`, `constraints-anchor-to-validated-artifact`, `planner-hint-advisory-only`, `unverified-skill-fab-risk-twin-pattern`, `eval-target-must-match-runtime-path`, `routing-verification-via-trace-not-latency`, `half-wired-schema-field-grep-whole-chain`
