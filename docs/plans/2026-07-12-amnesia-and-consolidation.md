# Amnesia fix + hardcode/prompt consolidation — execution plan

Written 2026-07-12 after the day's findings inverted the original premise. Everything
below is **verified** unless marked ⚠️UNVERIFIED. Read the "What we got wrong" section
first — most of the plan exists because of it.

---

## What we got wrong (read this before planning anything)

**The amnesia is NOT in the ReAct loop. It is in the intent router, upstream of it.**

`engine.go` `callPlannerOnce` says so in its own comment:

> PriorText is still passed for the validator's span check, **but buildUserPrompt no
> longer emits it** — PR1 hotfix Bug 2 (2026-05-28), because dumping the transcript grew
> multi-turn input until ds-v4-flash stopped returning schema-valid JSON.

So the router's **prompt contains no conversation at all**. It routes on: the current
message + `LastIntent` + `LastSelectedInstanceID` + `LastAssistantSnippet`. It is
effectively **stateless about the conversation**, and it runs **first** — it decides
which path the turn takes before the loop's memory is ever consulted.

Measured consequence (17 sessions, N=63 follow-ups, blind + position-randomised judge,
arm-neutral history):

| | fast path | agent loop |
|---|---|---|
| amnesia | 24/63 (**38.1%**) | 22/63 (**34.9%**) |
| wins | 23 | 24 (16 tie) |

**A dead heat.** Both arms share the same blind router. Therefore:

- the `DIRECT_DISPATCH_INTENTS=off` cutover does **not** fix amnesia;
- raising `maxHistoryMessages` 40→120 does **not** fix amnesia (the loop is downstream);
- ⚠️ **ExA's claim that the loop cut amnesia 29.7%→8.1% DID NOT REPRODUCE.** Suspect its
  judge built history from the candidate arm's own replies. (My first judge had the
  mirror bug — history built from *base*'s replies — and produced a fake 5–1 for the
  control. Do not trust any judge whose history is not arm-neutral.)

**What still stands from PR B:** deterministic rendering (0 fabricated instance IDs /
97 turns, vs 1 phantom without it), the three grounding-harvest fixes, the history
ceiling. These are real and worth merging. They are just **not the amnesia fix.**

---

## Committed so far (branch `feat/grounded-agent-loop`, base origin/main `83b3ee86`)

| commit | what |
|---|---|
| `d6fa5e81` | deterministic instance rendering + observe-only grounding (both flags default-OFF) |
| `231b4827` | history ceiling 40→120 (was sized for a 32K model we don't run); grounding harvests typed structs |
| `540d9144` | restored 2 trim tests the ceiling raise silenced; refuse to ground `json:"-"` fields |
| `5e173099` | harvest the payload SearchKnowledge actually returns, not a subset |
| `43576035` | **`observability.ContextTrace`** — per-turn record of what the router and loop could SEE |

Full suite green. **Nothing pushed** (the lead self-manages merges).

`ContextTrace` is the instrument the whole plan depends on. Counts and flags only, no raw
text, safe on in production. `RouterPriorInPrompt` is **always false today — that field
IS the finding.**

---

## PROGRAM A — the actual amnesia fix

### A1. Router prompt: let conversation state beat the few-shot saturation
**This is also item B7 (冗余 prompt). They are the same job.**

The router has **no hardcoded keyword map**. What it has is a prompt *saturated* with
`部署` → `deploy_model` few-shots (`internal/intent/router.go:437,452,489,544`) and no
conversation to override them. Result: 「我删了再部署」 — a user describing their own plan
mid-troubleshoot — becomes a create-instance flow. In production (mutating ON) that is a
**create-instance confirmation card**, not a polite refusal.

**Do NOT re-enable PriorText in the prompt.** That is precisely the 2026-05-28 avalanche.
`TestRouterAndLoopSeeDifferentContext` will go red if anyone does, and that red is the
signal to re-verify planner `schema_valid` on real multi-turn traffic.

**Use the signal that is already passed and currently ignored: `LastIntent`.** Add router
directives that make a *continuation* of a troubleshooting/knowledge intent outrank a
bare verb match. Prune the redundant `部署` few-shots at the same time (that is the
consolidation half).

Acceptance: on the real-traffic probe set, 「我删了再部署」-class turns stop routing to
`deploy_model`, **and** `schema_valid` does not regress. Both, or it does not ship.

### A2. Re-measure, with the harness fixed
Three things the last run got wrong, all mine:
1. **`_prB_server.ps1:41` sets `COMPSHARE_ENABLE_MUTATING_TOOLS=0`. Production ships `1`.**
   Every arm measured a config production never runs. **Set it to 1.**
2. Judge history must be **arm-neutral** (`_amnesia_judge.py` — user turns only). The
   biased version manufactured a 5–1 result.
3. Judge concurrency 3 + retries (the first run lost 10/16 to rate-limit).

Read `ContextTrace` alongside the verdicts. That is how you tell "it had the context and
ignored it" (prompt bug) from "it was never given it" (plumbing bug) — the two produce
identical replies and need opposite fixes.

### A3. Only if A1 is insufficient
Give the router a **structured conversation-state signal** (e.g. "this session is
mid-troubleshoot on instance X"), never the raw transcript. Structured signals are what
the 2026-05-28 hotfix replaced the transcript *with*; extend that, don't undo it.

### A4. Decide the cutover on its own merits
`DIRECT_DISPATCH_INTENTS=off` is **not** the amnesia fix. It may still be right (the loop
handles novel turns better), but it must now be justified by something other than memory,
and deterministic rendering is its precondition either way.

---

## PROGRAM B — consolidation (zero shipped to date; this is the debt)

Audited repo-wide. **No dead tables** — every one has a live reader. The problem is not
leftover demo code rotting quietly; it is **demo-era data that is still load-bearing and
now wrong.** Worse than dead: dead costs nothing.

Ranked by blast radius × staleness.

### B1. `gpuSpecs.FP16` — DELETE the column ⚠️ decision made, flag if you disagree
`internal/knowledge/gpu_specs.go:35`.

- **It is wrong.** The table's own contract says `FP16 口径统一为 Tensor Core dense peak`.
  RTX 4090 carries **82.6**, which is the **FP32 shader** figure; Tensor dense is
  **165.2**. RTX 3090 carries 35.6; Tensor dense is **71**. A100 carries 312, which *is*
  the genuine Tensor figure — so the 口径 is **mixed**, and 4090-vs-A100 comparisons are
  off by ~2×.
- **It inverts a recommendation.** `mostCapableAllowed` / `bestPerVRAM`
  (`model_specs.go:238,251,323`) rank by FP16. Table order puts V100S (32.8) below RTX
  3090 (35.6); on true Tensor-dense figures V100S (~130) beats a 3090 (71).
- **Nobody asks for it.** Of **1454 real user turns, ZERO** ask for 算力/TFLOPS (the only
  2 matches are 「创建一个算力实例」 and 「算力市场」). Users ask 显存 / 配置 / billing.
- **Upstream already supplies the rest.** `DescribeAvailableCompShareInstanceTypes` →
  `GraphicsMemory.Value` (VRAM), `Performance.Value` (perf score), `MachineSizes[].Gpu`.
  `gpu_live.go` already uses `Performance.Value` *"in place of the static FP16"* on the
  live path.

**Decision (mine — veto if you disagree):** delete the FP16 field. Live path ranks by
`Performance.Value` (already does). **Offline fallback ranks by VRAM only** — dropping
within-VRAM-tier preference on a degraded path is acceptable, and far better than a
tiebreak that is measurably backwards.

**⚠️ The fix must pull FOUR sites in one commit or grounding will flag the corrected
number as a hallucination — `82.6` got canonised:**
1. `gpu_specs.go:61` — the struct field
2. `gpu_specs.go:241` — a **customer-facing prose string**, an *independent literal*
3. `grounding.go:119` — the validator's design comment
4. `grounding_dump.go:32` — a comment asserting *"82.6 TFLOPS is the real 4090 FP16 figure"*

### B2. `GetGPURecommendation` scene tables — biggest blast radius
`gpu_specs.go:231-276`. Re-types TFLOPS into **customer-facing prose** as independent
literals (fixing the struct will NOT fix these). Missing **every card added since it was
written** — no 5090/5090D/H20/4090_48G — so cards we actively sell are invisible to scene
recommendations. And it **contaminates the live path**: `gpu_live.go:282`
`sceneCardAvailable` calls `GetGPURecommendation` and uses its ordering to pick from the
live pool, so the "live" recommender's scene priors are still the 2024 table.

Fix: strip TFLOPS from the reason strings; derive candidate cards from the live catalog;
keep only the scene *ordering* as curated data (that part is a judgment, not an API field).

### B3. `deployGPUAliases` — `deploy_model.go:1559`
14 regexes; its own comment calls it a mirror of `gpuSpecs`, so it drifts in lockstep.
Already subordinate to the live catalog (`extractDeployGPUFromCatalog` tries live names
first). **FALLBACK-OK** — but add a drift-guard test pinning its key set to the catalog.

### B4. `routing_pricing.go:204` — `zone = "cn-wlcb-01"` fallback
On a degraded response this **silently prices the wrong zone**. Replace with the live
`zones.Catalog`.

### B5. `resource_filter.go:141` — `if filter == "4090"` family special-case
Carries its own `TODO: derive the family relationship from the live catalog`. **5090/5090D
already have sub-variants — the TODO is already overdue.**

### B6. GPU card list, copies #3 and #4
`guardrails/topic.go:180` + `jailbreak.go:239`. A newly-launched card missing from these
allowlists makes a **genuine user question about it more likely to be refused**.

### B7. 冗余 prompt — **this is A1.** Not a separate task.
Production system prompt is 14,744 bytes / 6,974 runes (mutating mode) vs 5,132 / 2,196
read-only. Size measured; **harm NOT measured** — treat as an experiment through the
replay+judge harness, not an opinion.

### B8. 转人工 → planner intent (real user-facing bug)
`humanAgentTransferKeywords` (`engine.go:~6236`): 6-phrase substring whitelist,
hard-blocks pre-LLM in `preblock.go:60`. **Catches 39 of ~70 genuine requests = 56%
recall** on 2193 real turns. Top miss: bare 「人工」 (11×), bare 「客服」 (4×). **Widening the
list is NOT the fix** — 10 turns are pasted platform errors containing 「请联系客服」 and 4
are 人工智能.

Copy the `account_billing` precedent exactly: intent enum → router directives + few-shots
→ a `tryBillingAccountUnsupportedDispatch`-shaped dispatch fn → delete the `inputguard.Rule`.
**Eval set is free and pre-labelled from real traffic:** 39 must-fire / 31 must-now-fire /
10 must-NOT-fire (error pastes) / 4 must-NOT-fire (人工智能).

### Leave alone (verified legitimate)
`deployPreferredZones` / `deployZoneAliases` (documented degrade paths, correctly
subordinated to the live catalog); `serviceAliases` (soft-failing input normaliser, not a
fact table); diagnosis advice strings (generic, versionless); `ActionLevels`,
`readExpensiveDefaultActions` (our policy, not upstream's facts).

**The models to copy:** `canonicalModels` (3 entries, deliberately tiny so ambiguous
models *ask* instead of silently sizing), `builtinCapabilities` (every flag cites a probe
date + rollback), `checkableUnits` (documents each removal). The discipline exists in this
repo — the GPU table never had it.

---

## PROGRAM C — separate, not context, not consolidation

- **#427** (upstream conformance): do NOT merge as-is — 8 commits behind, 12 conflicts,
  and #420 duplicates its diagnosis half. But **4 findings are real user-facing bugs**:
  CFSUpgradePrice returns a **fake ¥0 "successful" price**; JupyterToken fails 100% for
  Pod instances; reset-password fails on Stopped Pods; a false "100GB free disk" promise.
  File them so they survive.
- **2C4G probe**: main (#432) sends `WithoutGpuSpec="A"` to every backend; #427 sends
  `"B"` for UCloud. **One of them is wrong on a mutating path.** One live probe settles
  it. (Lead's view: 2C4G/"A" is the sane default — but verify, don't assume.)
- **Misattribution** (`grounding` has no entity binding: `uhost-B 显存 24GB` reads clean
  when B has 12GB): **validator-only, observe-only, default-off, zero user impact.** I
  previously called this "the biggest problem" — that was me over-weighting my own
  tooling. Pinned by `TestKNOWNHOLE_MisattributedValueIsNotCaught`, which asserts the
  wrong behaviour on purpose. Do not delete it to get green.
- **P3 / P4 / P9: SETTLED, do not redo.** P3 (`inferLifecycleAction`) — leave it, it is
  the sole source of the lifecycle verb and its ordering hazard is defused on real traffic
  (12 collisions / 2193 turns, 10 caught by `looksLikeLifecycleQuestion`). P4 (zone
  literals) — **already fixed by #429**, verified against current main. P9
  (`serviceAliases`) — mis-filed, positive-only, one call site.

---

## Traps that cost real time today — do not repeat

1. **A validator you wrote is a hypothesis, not an oracle.** Its false positives look
   exactly like the subject's failures, and they are very easy to believe. Three harvest
   holes all reported the model's *correct* answers as fabrications. **Spot-check top
   flagged claims against ground truth (corpus / upstream API / code) BEFORE reporting
   validator output as a finding about the model.**
2. **Do not treat a hardcoded table as ground truth.** I sourced "82.6 is the real 4090
   figure" from our own wrong table and wrote it into the validator's docs as fact.
   Grounding is a **faithfulness** check, not a **correctness** check — it can only certify
   that the model faithfully repeated *our* data. It cannot make our data true.
3. **A/B judges must use ARM-NEUTRAL history.** Building history from one arm's replies
   rigs it for that arm. Mine produced a fake 5–1.
4. **Verify the LISTENING PID + path + start time** before trusting any live probe.
5. **A silent auth failure looks like a model failure.** prB's tracked `config.yaml` does
   not sign STS → every API call fails → 「查询接口暂时遇到故障」 with clean logs. Use
   `_prB_server.ps1`, and **always single-turn smoke for real data before a long run.**
6. **Test inputs go inert when you move a constant.** Raising `maxHistoryMessages` silently
   sailed 5 trim tests down the no-op branch: green, asserting nothing. Derive test inputs
   from the constant and `require()` that they actually overflow it.
7. **`deploy/conf/agent.yaml` in the prB worktree holds REAL CREDENTIALS, is untracked and
   NOT gitignored. Never `git add -A`. Stage by explicit path. Delete when done.**

## Environment

- Base `origin/main` = `83b3ee86`. Main checkout `F:\compshare-agent` is on
  `spike/agent-sdk-ssh-ops` — **NOT main**; do not grep it for main's code.
- Local postgres `cs-pg-runtime-fixes` on :3307. **Never** the remote DSN.
- `go test ./...` needs `COMPSHARE_PROJECT_ID=test-project`. Never set
  `COMPSHARE_ENABLE_MUTATING_TOOLS` in tests.
- Port 8080 = sshops-e2e, **do not touch**. Replay servers use 8100.
- Harnesses: `eval/realism/_prB_server.ps1` (boot), `http_session_replay.go` (replay),
  `eval/realism/grounding_score.go` (fabrication), `eval/realism/_amnesia_judge.py` (memory).
