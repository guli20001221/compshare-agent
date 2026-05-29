# B2b — Skill/Tool Directory Split + Codegen + Progressive Disclosure

**Date**: 2026-05-29
**Driver**: ADR-003 (skill ⊥ tool) + ADR-004 (skill bundle + progressive disclosure) + ADR-003 Amendment-1 (MCP naming/visibility). Expands the B2b *outline* in `docs/plans/2026-05-29-b2-router-inject-completion.md:165-198`.
**Audience**: implementer (Codex/Claude) + reviewer
**Estimated effort**: 2-3 weeks, phased (P1 ~3d, P2 ~4d, P-tool ~4d, P3 ~3d + validation)
**Status**: Draft rev-1 (grounded against code 2026-05-29; not yet reviewer-approved)
**Depends on**: B2a shipped (`b38fe28`, on main). **Blocks**: B4b planner-tier (needs the ≤3k prompt this delivers).

> This spec is grounded in a full current-state map (workflow `wf_611cd891`). Every "current" claim carries file:line. It supersedes the outline; where it diverges from the outline it says so.

---

## 0. Preconditions (resolve before any code)

1. **ADR-004 is `Proposed`, not Accepted** (`docs/adr/004-skill-bundle-structure.md:3`). Building the loader to its exact frontmatter/caution contract before ratification risks rework. **Freeze ADR-004's frontmatter schema + caution semantics** (or accept the rework risk) before P1.
2. **Tool-binding audit (resolved 2026-05-29):** the current capability layer has **four** binding sources, not the two ADR-003 codegen addresses:
   - (1) `capabilityRegistry` Go literal — `internal/intent/capability_registry.go:40-47` (intent→skillGroup→requiredTool→handler, the runtime dispatch source-of-truth)
   - (2) frontmatter `required_tool` (singular) — `capabilities/*.md`, reconciled to (1) by panic at init (`capability_registry.go:144-164`)
   - (3) `extraHandlerActions()` — `capability_registry.go:49-56` (stock's 2 extra security-gated tools)
   - (4) `IntentToolSubset()` — `internal/intent/tool_subset.go:6` (the ReAct-fallback-visible tool set per intent)
   - **Key correction:** (2) `required_tool` and (4) `IntentToolSubset` encode *different concepts* — the cutover handler's single deterministic tool vs the broader set the LLM sees if the turn falls through to ReAct. They are **not** drift; e.g. `gpu_specs` has `required_tool: DescribeAvailableCompShareInstanceTypes` but `IntentToolSubset = {DescribeAvailableCompShareInstanceTypes, GetGPUSpecs}` — and **`GetGPUSpecs` is a real registered tool** (`internal/tools/registry.go:11`), not phantom. Codegen therefore **cannot** derive `tool_subset` from `required_tools[]`; both must remain declared data. **Decision needed:** does the unified skill schema carry `tool_subset` as a separate field, or is the ReAct-fallback set dropped for migrated skills? (Recommend: keep as a declared `react_tool_subset[]` field — see §3.)

---

## 1. Why phased (P1 → P2 → P-tool → P3)

The outline bundles "directory move" + "codegen" + "prompt-shape change" + "tool spec" into one 2-3 week PR. That mixes a **zero-behavior-change** plumbing change with a **planner-routing-sensitive** prompt change, so a planner regression couldn't be bisected. Split into single-variable phases (memory `phase-a-strict-zero-behavior`, `smoke-test-cannot-infer-architectural-decision`):

| Phase | What | Behavior change | Validates against |
|---|---|---|---|
| **P1** | `loader.go` + `cmd/skillgen` + caution injection + CI scripts, against the **5 already-seeded `diagnose_*` skills** (data already on disk, no consumer today) | **none** (no routable consumer; loader exists but engine doesn't read it) | unit + `-race` |
| **P2** | Migrate the 6 active capabilities into `internal/skills/<name>/skill.md`; keep eager `CapabilityPromptFragments()` reading the **generated** metadata behind `USE_SKILL_LOADER=off` | **none with flag off** (byte-identical planner prompt) | SHA gate stays green + double-run diff |
| **P-tool** | `internal/tools/<tool>.json` + `x-compshare`, generator-validated **mirror** of `security.ActionLevels` (parallel to P2, disjoint surface) | **none** (generated `Registry` byte-rebuilds the current slice) | generator equality assertion + `go test` |
| **P3** | Flip to progressive disclosure: planner prompt = metadata-only, body-on-trigger; `USE_SKILL_LOADER=on` | **yes** (planner prompt bytes change) | N=20 ≤3k + N≥5 CLI jitter per intent |

Each phase ships independently with its own CLI smoke. P1+P2+P-tool are all zero-behavior-change (the B2a discipline); only P3 changes what the planner sees.

---

## 2. Current state (verified, file:line)

- **6 active capabilities**, all `skill_group: catalog`: `gpu_specs_query`, `stock_availability`, `platform_image_list`, `custom_image_list`, `community_image_list`, `pricing_query` (`internal/intent/capabilities/*.md`; `_general_tech_qa.md.disabled` excluded — `_`-prefix skip at `capability_registry.go:179`).
- **Two incompatible frontmatter dialects** (the core migration problem):
  - *Capability dialect* (`CapabilityMetadata`, `capability_registry.go:107-122`): `name, intent_label, skill_group, required_tool (singular), tool_subset, required_citation, planner_directives[], planner_examples[{question,confidence}]`. Body after `---`.
  - *ADR-004 skill dialect* (on disk in the 5 seeded skills, `diagnose_ssh/skill.md:1-19`): `name, description, triggers[], applicable_tiers[], required_tools[] (plural), related_skills[], body_cap_lines, verification_status, field_refs_verified`.
  - The 6 capabilities have **none** of `description/triggers/verification_status`; the skill dialect has **no** `planner_directives/planner_examples` (the Stage-2C routing anchors). A naive map onto the bare ADR-004 schema **drops routing signal** → silent planner regression.
- **`internal/skills/` already has 5 `diagnose_*` skill.md** (commit `b884312`) in ADR-004 dialect, **all `verification_status: unverified` + `field_refs_verified: false`**, but **no `loader.go`, no `cmd/skillgen`, no `registry_gen.go`, no `USE_SKILL_LOADER`** — markdown with zero Go consumer (typed grep clean).
- **Planner prompt assembly** (`buildSystemPrompt()`, `planner.go:520-527`): joins `base` (38-line classifier block, `planner.go:476-514`) + `renderPlannerPromptExampleGroups(...)` + capability `directives` + capability `examples`. All eager, every turn. `CapabilityPromptFragments()` (`capability_registry.go:251-303`) injects full directives + one synthesized Plan-JSON per example (**not** the body). Measured: **24,735 chars / 175 lines**; example groups dominate (113 lines / 12,335 chars). ~6k tokens, consistent with ADR's 5.9k.
- **`base` block (~30 global classifier directives, `planner.go:476-514`) is NOT skill-scoped** — it's global routing rules (CPU-high→monitor, finance-FAQ-vs-personal-status, etc.). ADR-004 progressive disclosure shrinks the *capability/example* segment, not `base`. After examples move out, `base` becomes the dominant residual → **≤3k may require a separate future directive-tiering effort** (flag, don't bundle).
- **SHA + structural gates that fire on any drift:**
  - `systemPromptSHA256Baseline = 1f6823...` pins the whole prompt; `TestPlannerExamples_FullSystemPromptStable` (`planner_examples_test.go:200-213`).
  - count assert `len(examples) == 28+capabilityExampleCount` (`planner_prompt_test.go:79`).
  - grouping assert: total **47** example questions, exact per-intent counts (resource_info 8, knowledge_qa 20, operation_lifecycle 6, disk_info 4, diagnosis 1...) (`planner_prompt_test.go:102-214`).
  - substring asserts: capability enum names + verbatim directive sentences (`planner_prompt_test.go:21-25` and the Routes* tests).
  - byte-equal disk-loader-equals-legacy (`planner_examples_test.go:62-69, 391-398`); YAML parsed with **`KnownFields(true)`** (`capability_registry.go:241`) → any new key hard-fails parsing unless the struct is extended in the same change.
- **Tool layer**: 31 tools as Go literals in one slice `var Registry []openai.Tool` (`registry.go:6`). Subset mapping is `IntentToolSubset()` in **`internal/intent/tool_subset.go`** (not `internal/tools/`). Mutation class is **runtime-derived** from `security.ActionLevels` (L0/L1/L2, `security/levels.go:28`) via `classForAction` (`policies.go:193`); `requires_acceptance` ≈ `NeedsConfirm = (level==L1)` (`policies.go:104`). **No `x-compshare`, no per-tool JSON, no codegen.** Two gates: visibility (`VisibleRegistry`, `registry.go:801` hides Workflow/mutating unless enabled) + execution (`safe_executor.go:189-194` always-refuse L2/destructive, refuse mutating when flag off). `COMPSHARE_ENABLE_MUTATING_TOOLS` parsed only at cmd layer (`"1"`=on else off+warn, `trace.go:159-169`).
- **Handlers are imperative Go** (renderers + envelope builders, e.g. `handleGPUSpecsQuery`/`renderGPUSpecsReply`/`buildGPUSpecsEnvelope`, `capability_registry.go:338-1069`) — codegen **cannot** eliminate them. The `handler` field is a func pointer (`capability_registry.go:37`).
- **Engine dispatch is already generic**: `IsCapabilityIntent` → `DispatchCapability` linear-scan (`capability_registry.go:61-99`); engine has one hook (`engine.go:961` uses `VisibleRegistryForSubset(IntentToolSubset(...))`). **No per-case engine change needed** to move the data layer.

---

## 3. Unified skill frontmatter schema (the load-bearing decision)

Define **one** schema before moving any file — a **superset** of both dialects so the catalog capabilities' routing anchors survive and the loader can read everything:

```yaml
# internal/skills/<name>/skill.md
---
name: gpu_specs_query              # snake_case [a-z][a-z0-9_]*[a-z0-9], 1-64; dir name MUST equal this
description: "..."                  # one line, planner-visible (ADR-004)
triggers: ["...", "..."]           # ADR-004 classification anchors (advisory-only)
applicable_tiers: [fast]           # fast | knowledge | agent
required_tools: [DescribeAvailableCompShareInstanceTypes]  # plural; tools the handler/skill calls
react_tool_subset: [DescribeAvailableCompShareInstanceTypes, GetGPUSpecs]  # NEW: ReAct-fallback-visible set (≠ required_tools; see Precondition 2)
related_skills: []
body_cap_lines: 100
verification_status: production_validated   # the 6 migrated are live + field-checked
field_refs_verified: true
# --- routing block (carried over from capability dialect so Stage-2C signal survives) ---
intent_label: gpu_specs_query      # the Intent enum this skill dispatches
skill_group: catalog
required_citation: false
handler_key: handleGPUSpecsQuery   # name → func map entry (existence checked at generate time)
planner_directives: ["..."]        # imperative routing rules (move to body/examples in P3, see below)
planner_examples: [{question: "4090 显存多大", confidence: 0.85}]
---
<markdown body — 正例/反例/边界, ≤100 lines>
```

Rationale:
- **`required_tools` plural** absorbs the singular `required_tool` (map `required_tool → [that]`). The 6 migrated set `verification_status: production_validated`, `field_refs_verified: true` (they are live, API-field-checked).
- **`react_tool_subset` is new and explicit** — Precondition 2 proved it can't be derived. Codegen emits `IntentToolSubset` from this field; `extraHandlerActions()` folds in as `required_tools[1:]` (primary = `required_tools[0]`).
- **`planner_directives`/`planner_examples`/`handler_key` widen the ADR-004 schema** — surface to ADR-004 owners as a schema amendment (it currently has only `triggers[]`). Without this the planner loses the directives the Routes* tests pin.
- All 6 dir names are already snake_case (`gpu_specs_query`...`pricing_query`) → **zero rename churn**.

---

## 4. Phase P1 — loader + codegen + caution + CI (zero behavior change)

Build the machinery against the 5 already-seeded `diagnose_*` skills (they are the natural unverified-path fixtures; no routable consumer, so no behavior change).

- `internal/skills/loader.go` (~300 lines, ADR-004:108-137): `NewLoader(root)` scans `root/*/skill.md`, parses **frontmatter only** (reuse the `---` splitter at `capability_registry.go:225-247`), body unread. `Skill.Body()` guarded by `sync.Once` does the read, strips frontmatter, enforces `body_cap_lines` (default 100, ceiling 200, **build/load fail not truncate**), and — single choke point — prepends `[CAUTION: this methodology is unverified, treat steps as suggestions not facts]` when `verification_status==unverified` and appends `[FIELD REFS NOT VERIFIED — confirm field names against actual API response before action]` when `field_refs_verified==false` (ADR-004:81,104). `Metadata() []SkillMeta` projects name+description+triggers (~80 tok/skill). `Fetch(name)`.
- **Strict parsing** (CLAUDE.md flag convention): missing/empty/unknown `verification_status`/`field_refs_verified` → hard-fail at `NewLoader`, never default to permissive (ADR-004:88 "默认值不允许").
- `cmd/skillgen` (~150 lines, ADR-004:170): `go:generate go run ./cmd/skillgen --root internal/skills --out internal/skills/registry_gen.go` → `var generatedSkills map[string]*Skill`. **Generate-time existence check** that every `handler_key` resolves in a hand-maintained `handlerByName` dispatch map (mirrors the current init panic at `capability_registry.go:144-164`, so a missing handler fails `go generate`, not runtime).
- **Digest pin (stronger than ADR-004's "git status clean"):** add `registry_gen.go` (or the LF-normalized concatenated frontmatter) to a `sha256` pin analogous to `internal/knowledge/corpus_digest.go`; CI runs `go generate && git diff --exit-code` **and** verifies the digest. LF-normalize before hashing (this repo is CRLF on Windows — same footgun as the corpus pins).
- CI: `scripts/check_skill_names.sh` (snake_case + dir==name) + `scripts/check_skill_caps.sh` (body-only line count, skip frontmatter, fail >cap) into `.githooks/pre-commit` alongside `secret_scan.ps1` (hook already requires PowerShell → ship `.ps1` variants).
- **Tests**: loader unit (lazy-body, frontmatter-skip count, name==dir, caution injection for both flags), **`sync.Once` concurrent-`Fetch` `-race`** (add `internal/skills` to a `-race` CI target — today only `internal/entity` is raced, and `sync.Once` is the named ADR-004 risk), body-cap build-fail fixture, codegen drift (`git`-clean).

P1 acceptance: `go test ./... ` green, the 5 skills load with both caution lines, **engine unchanged** (no consumer wired).

---

## 5. Phase P2 — migrate the 6 capabilities (zero routing change, flag off)

- Move each capability to `internal/skills/<name>/skill.md` in the §3 unified schema; body = existing markdown (all under 100 lines — `pricing_query` longest ~52).
- Introduce `USE_SKILL_LOADER` (**default off**, env/config-only, unknown→off+warn per CLAUDE.md). Behind it, a generated `skillRegistry` parallels `legacyCapabilityRegistry`. `IsCapabilityIntent`/`DispatchCapability` and `CapabilityPromptFragments()` read whichever the flag selects.
- **Flag-off invariant (phase-a-strict-zero-behavior):** with the flag off, the planner prompt and dispatch are **byte-for-byte** the legacy path. `systemPromptSHA256Baseline` stays pinned to legacy and `TestPlannerExamples_FullSystemPromptStable` stays green **untouched**. Add a test asserting flag-off == legacy registry byte-for-byte.
- Keep the registry↔frontmatter parity asserts as the migration net, re-pointed at the generated registry: `TestHandlerActionWhitelist_DerivesFromRegistry`, `TestCapabilityRegistry_BindsToRealTool`, `TestIntentToolSubset_CapabilityIntents` (counts `{gpu_specs:2, stock:3, pricing:2, image-lists:1}`) must stay green through cutover (re-point, don't rewrite).

---

## 6. Phase P-tool — tool JSON + x-compshare (parallel, zero behavior change)

Disjoint surface from P2 (`internal/tools` vs `internal/intent`), runnable in parallel.

- One `internal/tools/<tool_name>.json` per tool (31) = OpenAI `FunctionDefinition` + `x-compshare` block. `go:generate` rebuilds `var Registry []openai.Tool` byte-for-byte so `engine.go:961` and `policies.go:223` callers are **unchanged**. JSON becomes the single source; Go literals deleted.
- **`x-compshare` is a generated/validated MIRROR of `security.ActionLevels`, not a hand-authored parallel source** (drift is the #1 ADR-003 risk). Generator asserts, per tool: `x-compshare.requires_acceptance == (ActionLevels[name]==L1)` and `x-compshare.class` maps to the same `ActionClass` `classForAction` produces — **build fails on mismatch**. `security/levels.go` stays the authoritative gate during B2b; invert authority (JSON primary) only in a later PR after a green release.
- **L2/destructive gap (ADR-003 enum has only `read|mutating`):** map L0→`{class:read, requires_acceptance:false}`, L1→`{class:mutating, requires_acceptance:true}`, L2→`{class:mutating, requires_acceptance:true, destructive:true}` — propose adding `destructive` as **Amendment-2** so `safe_executor.go:189`'s always-refuse-L2 is reconstructable from the spec. Without it the L2 always-refuse regresses to a mere mutating-flag check.
- **`tier_eligible` ships default-off/advisory** (memory `planner-hint-advisory-only`, `new-protocol-frame-default-off`); `IntentToolSubset` stays the live routing source. Reconcile subset-vs-tier in a separate ADR. Do **not** build `internal/mcp/naming.go`/`projection.go` — explicitly B7 (ADR-003:174,194). B2b only ensures `x-compshare` field names match the Amendment-1 visibility table so the future projection is a pure filter.
- Generator enforces the Amendment-1 checklist (ADR-003:207): build-fails if any `<tool>.json` omits any `x-compshare` key.

---

## 7. Phase P3 — progressive disclosure (the only behavior change)

- Replace the eager capability section of the planner prompt with `renderSkillCatalog(loader.Metadata())` — one `name — description | triggers` line per **routable** skill; drop the synthesized per-example Plan-JSON from the planner prompt (few-shot anchors move into a sibling `examples.jsonl`, used only for agent-tier execution if ever).
- Add `Skills []string` to the `Plan` schema + validator (max 3, ADR-004:162) so the planner emits chosen skills. `Engine.Chat()` calls `loader.Fetch(name).Body()` in priority order with the **~2K-token first-fit budget** (N=2 default, N=3 hard, warning on overflow, ADR-004:159-165) and injects bodies **only into the agent-tier system prompt** — never the planner prompt, never fast/knowledge (ADR-004:144). **This is the avalanche guard** (memory `priortext-avalanche-invalidates-planner`): injecting bodies into the wrong path re-creates the 5k→11k blowup. Enforce at the engine injection point + test that fast/knowledge never call `Body()`.
- **Exclude `unverified`/`spike`-only skills from planner metadata** until `production_validated` — so the ≤3k measurement reflects only production-routable skills (memory `constraints-anchor-to-validated-artifact`) and unverified methodology never silently enters the prompt. The 5 `diagnose_*` stay route-inert.
- **SHA baseline at flip:** re-pin `systemPromptSHA256Baseline` **only** in the P3 PR that flips `USE_SKILL_LOADER=on`, with per-skill justification. Add a **second** baseline constant (`systemPromptSHA256BaselineSkillLoader`) so both paths are pinned during the dual-run window.

---

## 8. Double-run safety + cutover (resolves the outline contradiction)

The outline says "dispatcher reads whichever flag selects" (one path/turn) **and** "compare two paths side-by-side" — a single-flag dispatcher can't run both in one turn, and claiming "both" violates memory `attribution-observable-only`. **Resolution: two-pass diff, not per-turn shadow.**

1. Ship P1+P2+P-tool with `USE_SKILL_LOADER=off`, both paths build.
2. Run the CLI/HTTP smoke suite **twice** — pass A (flag off, legacy), pass B (flag on, skillRegistry) — and diff the trace sets. The trace field records **only** the path that actually served each turn.
3. **Split the gate:**
   - **Deterministic (hard, 0-violation, memory `hard-contractual-gates-binary`):** for every turn, capability dispatch target + handler output + selected `required_tools` are **byte-identical** across passes (cutover handlers run without LLM → deterministic). Reuse the `legacyXGroup` deep-equal + byte-equal-render pattern (`planner_examples_test.go:45-80`).
   - **Statistical (reported, not gated at 100%):** planner intent-classification agreement across passes, N=20, reported as % (LLM jitter, memory `jitter-check-for-classification`).
4. Flip `USE_SKILL_LOADER=on` (config-only, no redeploy); keep legacy **2 weeks** as instant rollback. **Rollback trigger** (was unspecified): any deterministic-gate violation in production traces, or planner ≤3k regression, flips the flag off — config-only.
5. Delete `legacyCapabilityRegistry` in a B2b-followup PR after the window with no rollback.

---

## 9. Acceptance (merged ADR-003 + ADR-004 + Amendment-1)

- [ ] `internal/skills/` holds all **6 active capabilities** as `<name>/skill.md`, snake_case `name`==dir (ADR-004:218).
- [ ] Every `skill.md` (incl. the 5 pre-seeded `diagnose_*`) carries `verification_status` + `field_refs_verified`, no defaults, CI-existence-checked (ADR-004:88).
- [ ] `internal/skills/loader.go`: lazy `Body()` + `sync.Once` + unit tests **incl. a concurrent-`Fetch` `-race` test** (ADR-004:219).
- [ ] `cmd/skillgen` produces `registry_gen.go`; `go generate ./...` clean-tree + digest-pin check in CI (ADR-004:220).
- [ ] Progressive disclosure: planner prompt = `Metadata()` only; body fetched on trigger; **test asserts fast/knowledge never call `Body()`** (ADR-003:91, ADR-004:144).
- [ ] `internal/tools/<tool>.json` for each bound tool with `x-compshare` 5 fields filled + Amendment per-field MCP-visibility note; generator equality-asserts against `security.ActionLevels` (ADR-003:89,207).
- [ ] `scripts/check_skill_caps.sh` (frontmatter-skipping, fail >200, default 100) + `scripts/check_skill_names.sh` in `.githooks/pre-commit` (ADR-004:221,69).
- [ ] ≥1 cross-tier-reuse skill (`safety_warning`) referenced by both a fast mutating-workflow and agent `deploy_model` (ADR-004:222).
- [ ] N=20 sampling proves planner prompt **≤3k tokens** (record the number; gate on ≤3k) (ADR-004:223). *If `base`-block residual blocks ≤3k, file directive-tiering as a follow-up and report the measured floor — do not lower the gate (memory `hard-contractual-gates-binary`).*

---

## 10. Test plan (memory `cli-regression-for-capability-changes`)

1. `go test ./... -count=1` green incl. SHA + `planner_examples` + leak-audit (unchanged through P1/P2/P-tool).
2. Loader unit + `-race`; body-cap build-fail fixture; frontmatter-skip count; name==dir; caution injection (both flags).
3. Codegen drift (`go generate && git diff --exit-code`) + digest pin.
4. **Two-pass CLI smoke** over all 6 capability intents + the `IntentDiskInfo` planner-only case + ≥1 zero-target `operation_lifecycle` (confirm no accidental skill capture). **Mandatory** — `go test` is insufficient for capability changes.
5. HTTP smoke parity (the B2a 4-turn matrix in `eval/traces_server_local`), flag-off vs flag-on.
6. P3 only: N≥5 same-question jitter per capability intent (memory `jitter-check-for-classification`) + N=20 ≤3k measurement on the **production-routable** skill set.

---

## 11. Risks

- **Routing-signal loss** if the schema is narrowed to bare ADR-004 (no `planner_directives`) — the Routes* substring tests fire and planner classification of the 6 intents silently regresses. Mitigation: §3 superset schema + N≥5 CLI per intent.
- **SHA-baseline waves**: any frontmatter byte-format drift fires `TestPlannerExamples_FullSystemPromptStable`. Mitigation: codegen-emit frontmatter deterministically; flag-off keeps legacy pinned; re-pin only at P3 flip; dual baseline during dual-run.
- **Unverified bodies entering the prompt**: the 5 `diagnose_*` are `unverified`+`field_refs_verified:false` and cite concrete API fields. Mitigation: exclude from routable metadata until validated + the loader's two caution lines (single choke point).
- **Mutation-gate regression** if `x-compshare` is hand-authored: an L1 tool marked `class:read` becomes LLM-visible + executes without confirm. Mitigation: generator equality-assert against `security.ActionLevels`, build-fail on mismatch. Preserve `routeForAction` name-suffix derivation (else `*Workflow` tools leak into read-only mode, `registry.go:812`).
- **L2 always-refuse loss** if mapped to bare `mutating`. Mitigation: `destructive` field (Amendment-2).
- **`sync.Once` concurrency** unraced unless `internal/skills` joins a `-race` target.
- **`KnownFields(true)` parsing**: any new YAML key hard-fails unless the struct is extended in the same change.
- **≤3k unreachable from metadata alone**: `base` block (~30 global directives) is the residual; report the floor, file directive-tiering follow-up, don't lower the gate.

---

## 12. Open decisions (need ADR amendment / lead sign-off)

1. **Schema widening**: ADR-004 frontmatter gains `planner_directives`, `planner_examples`, `handler_key`, `react_tool_subset`, `intent_label`, `skill_group`, `required_citation` (carried from capability dialect). Amend ADR-004 or accept routing-signal loss.
2. **`destructive` x-compshare field** (Amendment-2) to preserve L2 always-refuse from spec.
3. **`react_tool_subset` as declared data** vs dropping the ReAct-fallback set for migrated skills (Precondition 2).
4. **ADR-004 ratification** (currently `Proposed`) before P1, or accept rework risk.
5. **≤3k gate floor**: if `base`-block residual blocks ≤3k, is directive-tiering in-scope for B2b or a follow-up?

---

## 13. Refs

- ADR-003 (skill ⊥ tool + Amendment-1 MCP), ADR-004 (bundle + progressive disclosure), ADR-005 (the 5 diagnose skills' provenance)
- B2a spec `docs/plans/2026-05-29-b2-router-inject-completion.md` §B2b (outline this expands)
- Roadmap `docs/plans/roadmap.md` (B2b = critical path)
- Current-state map: workflow `wf_611cd891` (groundwork, 2026-05-29)
- Memory: `phase-a-strict-zero-behavior`, `new-protocol-frame-default-off`, `hard-contractual-gates-binary`, `attribution-observable-only`, `jitter-check-for-classification`, `cli-regression-for-capability-changes`, `priortext-avalanche-invalidates-planner`, `constraints-anchor-to-validated-artifact`, `planner-hint-advisory-only`, `unverified-skill-fab-risk-twin-pattern`
