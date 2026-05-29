# B2 — Router Inject Completion + Skill/Tool Dir Foundation

**Date**: 2026-05-29
**Driver**: ADR-002 Acceptance #3 (engine.go:296 + golden_test.go:684 + evaluate_test.go:180 inject point migration) + ADR-003 (skill/tool 目录拆) + blueprint B2 batch
**Audience**: Codex (实施) + Claude (reviewer)
**Estimated effort**: B2a 1-2 days / B2b 2-3 weeks
**Revision**: 3 (2026-05-29, post-implementation) — review round 2 + implementation verified against code, fixed 4 more findings:
- **B-6** (caller-graph): the rev-2 "Option B = add `*llm.Router` parameter" premise is false for 2 of 3 real callers. `NewSharedDeps` is called by `engine.New` (engine.go:356, used by `cmd/cli.go:82`), `agentpool.New` (pool.go:82, test-only), and `buildHTTPServerPool` (shared_deps.go:16) — **not** `cmd/cli.go` directly. `engine.New` / `agentpool.New` live in `internal/` and can't import `cmd`'s `buildLLMRouter`, and hold no pre-built router. **Resolution: build-inside, signature UNCHANGED** — `NewSharedDeps(cfg)` builds the router internally. Zero caller churn.
- **B-6b** (nil-overrides, no helper): build with `llm.NewRouter(cfg.Agent.LLM, nil)` — **not** a `NewRouterFromConfig(cfg)` helper. The router is transient (only `For(TierFast)` is extracted, then discarded — no `SharedDeps.Router` field per YAGNI), so it never survives to B4; honoring `tier_routing.fast` for the main loop now would be a behavior change (main loop handles all intents pre-B4, ADR-002:79) violating the zero-behavior-change mandate + memory `phase-a-strict-zero-behavior`. nil overrides → `For(TierFast)` == base for **every** config (not just empty `tier_routing`). No helper is created; `cmd/shared_deps.go::buildLLMRouter` is untouched.
- **B-7** (empty-model fixture + allowed change): `llm.NewRouter` rejects empty `base.Model` (router.go:61, fail-loud by design), but the old `llm.NewClient` tolerated it. `internal/agentpool/pool_test.go::minimalConfig` set `Model: ""` intentionally → now panics via `agentpool.New` → `NewSharedDeps` error. Fixed the fixture to a non-empty placeholder. This is an **allowed change** (memory `acceptance-invariant-with-allowed-change`): NewSharedDeps now validates the model at construction instead of at first LLM call. Only this one fixture had an empty model (swept all `Model: ""`).
- **B-8** (byte-stable test already exists): the rev-2 acceptance named `TestNewSharedDeps_RouterDerivedClient_ByteStableWithEmptyTierRouting` asserting "(BaseURL, Model, APIKey) triple" — **not implementable** because `*llm.Client` has unexported fields and no accessors. The byte-stable invariant is **already pinned** by `internal/llm/router_test.go::TestNewRouter_NilOverrides_AllTiersUseBaseModel`. Replaced with two engine-level tests in `internal/engine/shared_deps_router_test.go`: `TestNewSharedDeps_EmptyBaseModel_ReturnsError` (pins the B-7 allowed change) + `TestNewSharedDeps_LLMClientIsRouterDerived` (asserts `*llm.Client` wiring type).
- **B-9** (grep gate count): the rev-2 gate `grep "llm\.NewClient\(" ... --exclude=*_test.go --exclude-dir=ocr` expected "2 (router.go + cli.go)", but `router.go:82` uses **bare in-package** `NewClient(effective)` which the `llm.`-qualified pattern never matches. True result after B2a is **exactly 1: `cmd/cli.go:349`** (B4-deferred planner). The Router factory's own source (`router.go:82`) is the single allowed construction site and is bare-qualified.

Rev 2 (2026-05-29) — review round 1 fixed 5 findings: grep invariant self-contradiction (B-1), Option A pseudo-code type error (B-2), missing leak-audit cost (B-3), double-build wording (B-4), eval skip mechanism imprecision (B-5). Implementation path switched from Option A → Option B per reviewer-recommended YAGNI scope.

## Why split B2 into B2a + B2b

Per blueprint B2 batch (3 weeks) bundles two fronts:

1. **Router inject completion** — replace remaining 2 product `llm.NewClient(cfg)` callsites in B2a scope with `Router.For(tier)` (engine internal + 2 eval). `cmd/cli.go:349` planner stays as the third site, **explicitly B4-deferred** per ADR-002 Risks #3 (planner-on-flash blocked on ADR-004 progressive disclosure). Pure structural refactor for the 2 B2a sites, **zero behavior change**, no prompt content drift.
2. **Skill/Tool directory split + codegen** — migrate `internal/intent/capabilities/*.md` (6 active) into `internal/skills/` + `internal/tools/` with `go generate` codegen replacing `capability_registry.go` Go literal array. **Prompt content migration**, requires double-run safety net + N=20 regression vs current behavior.

Bundling them means a single 2-3 week PR that mixes zero-risk plumbing with high-risk prompt churn. Splitting:

- **B2a** (this spec, ready now) — Router inject completion for engine + 2 eval sites. Ships first, unblocks B4 (engine tier-aware) without waiting for skill loader.
- **B2b** (separate spec, drafted after B2a merges) — Skill/Tool dir + codegen, double-run safety. Maps to ADR-003 Acceptance.

Each ships independently with its own CLI smoke + regression budget.

---

## B2a: Router Inject Completion

### Goal

Migrate the engine-internal + 2 eval `llm.NewClient(...)` callsites to consume `Router.For(TierFast)`. After B2a ships, the remaining direct LLM-client constructions in product code are:

| File:line | Form | Status after B2a | Why still present |
|---|---|---|---|
| `internal/llm/router.go:82` | bare `NewClient(effective)` (in-package) | unchanged | Router internal — canonical single-source factory; THE allowed construction site |
| `internal/ocr/client.go:27` | `llm.NewClient(...)` | unchanged | OCR — NOT migrated per ADR-002:82 (independent path; gate excludes via `--exclude-dir=ocr`) |
| `cmd/cli.go:349` | `llm.NewClient(cfg.Agent.LLM)` | **unchanged in B2a** | Planner — B4-deferred per ADR-002 Risks #3 (planner-on-flash requires ADR-004 progressive disclosure first; otherwise priortext-avalanche per memory) |

Grep gate (qualified pattern, products only, excl. tests + ocr): `grep -rnE "llm\.NewClient\(" cmd/ internal/ eval/ --include="*.go" --exclude="*_test.go" --exclude-dir=ocr` → **exactly 1**: `cmd/cli.go:349`. B4 removes it → 0. (`router.go:82`'s bare in-package `NewClient(` is the factory source and never matches the `llm.`-qualified pattern; ocr is dir-excluded.) See **B-9** in the Revision header.

Test files (`*_test.go`) under `eval/` also lose their direct `llm.NewClient(...)` calls — only `golden_test.go:684` and `evaluate_test.go:180` are in B2a scope; verified no other test site constructs an LLM client outside `internal/llm/client_test.go` (the Client's own unit tests, legitimately direct).

CLI + HTTP behavior is byte-stable: empty `tier_routing` in config means all tiers point at the base `cfg.Agent.LLM` (already B1 invariant, ADR-002 Acceptance #5).

### Acceptance

- [x] `internal/engine/engine.go:296` migrated: `LLMClient LLMClient` interface field UNCHANGED; `NewSharedDeps(cfg)` **signature UNCHANGED** (B-6), builds the router internally via `llm.NewRouter(cfg.Agent.LLM, nil)` (B-6b) and assigns `router.For(llm.TierFast)` (a `*llm.Client`) into the field
- [x] `eval/golden_test.go:684` `llm.NewClient(config.LLMConfig{...})` → `llm.NewRouter(config.LLMConfig{...}, nil)` + `Router.For(TierFast)`. nil overrides → byte-identical to the base model. Pre-existing `t.Skipf` no-API-key skip (~line 681) still fires first
- [x] `eval/evaluate_test.go:180` same pattern; the `t.Logf + continue` no-API-key skip (~lines 175-176, outside the `t.Run`) still fires first
- [x] Product grep gate: `grep -rnE "llm\.NewClient\(" cmd/ internal/ eval/ --include="*.go" --exclude="*_test.go" --exclude-dir="ocr"` returns **exactly 1** line: `cmd/cli.go:349` (B4-deferred planner). `router.go:82`'s bare in-package `NewClient(` is the factory source (not `llm.`-qualified); cli.go:349 removed in B4 → 0. See **B-9**
- [x] `go test ./... -count=1` green — `TestSharedDeps_NoMutatingSetterLeakage` + `TestSharedDeps_AuditCoversAllSharedDepFields` (`internal/engine/shared_deps_leak_audit_test.go`) stay green WITHOUT modification: build-inside adds no SharedDeps field, and `router.For(TierFast)` returns the same `*llm.Client` concrete type already in `sharedDepConcreteTypes`
- [x] Backward-compat byte-stable invariant (empty `tier_routing` → `For(TierFast)` == base model) pinned by existing `internal/llm/router_test.go::TestNewRouter_NilOverrides_AllTiersUseBaseModel` (B-8 — the engine-level triple-compare test was infeasible: `*llm.Client` is opaque). New engine-level guards in `internal/engine/shared_deps_router_test.go`: `TestNewSharedDeps_EmptyBaseModel_ReturnsError` (pins the B-7 allowed change) + `TestNewSharedDeps_LLMClientIsRouterDerived` (asserts the `*llm.Client` wiring type)
- [x] `internal/agentpool/pool_test.go::minimalConfig` model fixed `"" → "test-model"` (B-7) — `llm.NewRouter` rejects empty `base.Model`; swept all `Model: ""` and this was the only one reaching the Router
- [x] HTTP smoke (2026-05-29) — 4 turns on the B2a binary (resource_info / pricing_query / knowledge_qa / operation_lifecycle-stop) compared against the same-day pre-change main-binary turns in `eval/traces_server_local/agent-trace-2026-05-29.jsonl`: `intent` / `model` / `tools` / `cutover_status` / `confidence` **IDENTICAL** across all four. `model=deepseek-v4-flash` throughout confirms `For(TierFast)`==base. Mutating stop (`StopCompShareInstance:success`) works end-to-end; the stop turn's `intent=unknown` is the pre-existing zero-target planner gap (matches old turn #11), not a B2a regression. NB: these HTTP trace records carry no top-level `task_tier` field — a B1/B4 trace-wiring matter (ADR-002 Acceptance #4, server path), orthogonal to B2a which makes no trace-schema change.

### Implementation: build-inside (signature unchanged)

Keep the `SharedDeps.LLMClient LLMClient` interface field **and the `NewSharedDeps(cfg)` signature** unchanged. Build a base-only Router **inside** `NewSharedDeps` and assign `router.For(TierFast)` (a `*llm.Client`, satisfying the existing `LLMClient` interface in `engine.go:107`) into the same field. This realizes the rev-2 "Option B" intent (no `Router` field on `SharedDeps`) correctly for the actual caller graph (B-6).

```go
// internal/engine/engine.go — signature UNCHANGED
func NewSharedDeps(cfg *config.Config) (*SharedDeps, error) {
    if cfg == nil {
        return nil, errors.New("engine.NewSharedDeps: cfg is nil")
    }
    // nil overrides → For(TierFast) == cfg.Agent.LLM, byte-stable for every config.
    router, err := llm.NewRouter(cfg.Agent.LLM, nil)
    if err != nil {
        return nil, fmt.Errorf("engine.NewSharedDeps: build LLM router: %w", err)
    }
    cap := llm.LookupCapability(cfg.Agent.LLM.BaseURL, cfg.Agent.LLM.Model)
    return &SharedDeps{
        LLMClient: router.For(llm.TierFast),  // *llm.Client satisfies LLMClient
        // ... rest unchanged ...
    }, nil
}
```

Why nil overrides and no `NewRouterFromConfig(cfg)` helper (B-6b): the router built here is **transient** — only `For(TierFast)` is read out, then it's discarded (no `SharedDeps.Router` field, YAGNI). It never survives to B4, so building it config-aware buys nothing while risking a behavior change: honoring `tier_routing.fast` would bind the main ReAct loop (which handles *all* intents until B4) to the fast model, misrouting knowledge/agent work (ADR-002:79). nil overrides → `For(TierFast)` == base for **every** config, not just empty `tier_routing` — the strongest reading of B2a's zero-behavior-change mandate (memory `phase-a-strict-zero-behavior`). `cmd/shared_deps.go::buildLLMRouter` is left untouched; no helper is added.

**What this does NOT do**:
- Does NOT change the `NewSharedDeps` signature — `engine.New` / `agentpool.New` / `buildHTTPServerPool` are all untouched (B-6: 2 of 3 are `internal/` packages with no pre-built router)
- Does NOT add a `Router` field to `SharedDeps` — no consumer until B4 (memory `phase-a-strict-zero-behavior` + Rule 2)
- Does NOT change the `LLMClient` interface type — all 12 `LLMClient: &mockLLM{}` / `<var>` injection sites under `internal/engine/*_test.go` keep compiling unchanged; leak audit `sharedDepConcreteTypes` (`*llm.Client`) unchanged
- Does NOT honor `tier_routing.fast` for the main loop — that's B4

### Caller graph (B-6, verified against code)

| Caller | File:line | Change |
|---|---|---|
| `engine.New(cfg, confirmFn)` | engine.go:376 (used by `cmd/cli.go:82` + `eval/capability/monitor_stale_reuse_probe_test.go:177`) | none — calls `NewSharedDeps(cfg)`, signature unchanged |
| `buildHTTPServerPool` | cmd/shared_deps.go:15 | none |
| `agentpool.New` | pool.go:81 (test-only; HTTP uses `NewWithDeps`) | none |
| `NewSharedDeps(nil)` test | engine_session_test.go:388 | none |

(Line numbers above are post-change — the ~20 doc-comment lines this change added to `NewSharedDeps` shifted `engine.New` from 356→376; symbol names are the stable reference.)

`cmd/cli.go` does **not** call `NewSharedDeps` directly (rev-2 spec error) — it goes through `engine.New`.

### Why not the rejected alternatives

- **Option A (add `Router` field to `SharedDeps`)** — rejected in rev-1: leak-audit drift (`shared_deps_leak_audit_test.go:91` `sharedDepConcreteTypes` + `:142` `nonAuditableFields` two-place sync) + speculative wiring unused until B4. Build-inside keeps the field set identical, so `TestSharedDeps_NoMutatingSetterLeakage` + `TestSharedDeps_AuditCoversAllSharedDepFields` stay green untouched (verified).
- **Signature change `NewSharedDeps(cfg, router)`** (rev-2 "Option B") — rejected in rev-2 review (B-6): the premise that callers already hold a router is false for `engine.New` / `agentpool.New` (internal packages, no router, can't import `cmd.buildLLMRouter`); it would cascade a 2nd signature change up to `cmd/cli.go:82` or force those callers to build a router anyway (the very "3rd build" the premise said it avoided). 4 caller sites churned vs 0 for build-inside.

### Files touched (as implemented)

| File | Change | Lines |
|---|---|---|
| `internal/engine/engine.go` | `NewSharedDeps` body only: `llm.NewRouter(cfg.Agent.LLM, nil)` + error wrap + assign `router.For(llm.TierFast)` into the existing `LLMClient` field. **Signature unchanged.** | ~5 + doc comment |
| `internal/engine/shared_deps_router_test.go` (new) | `TestNewSharedDeps_EmptyBaseModel_ReturnsError` (B-7 allowed change) + `TestNewSharedDeps_LLMClientIsRouterDerived` (`*llm.Client` wiring). Byte-stable model identity covered by existing `router_test.go` (B-8). | ~60 new |
| `internal/agentpool/pool_test.go` | `minimalConfig` `Model: "" → "test-model"` + comment (B-7). | ~2 change |
| `eval/golden_test.go` | `llm.NewRouter(...)` + `Router.For(TierFast)` instead of direct `NewClient`. `t.Skipf` no-API-key skip (~line 681) preserved before the Router build. | ~8 change |
| `eval/evaluate_test.go` | Same pattern. `t.Logf + continue` no-API-key skip (~lines 175-176) preserved — it sits in the `for` loop, before the `t.Run` where the Router is built. | ~8 change |
| `docs/plans/2026-05-29-b2-router-inject-completion.md` | This rev-3 update (B-6..B-9). | — |

**NOT touched** (rev-2 wrongly listed these): `cmd/cli.go`, `cmd/shared_deps.go`, `cmd/server.go`, `internal/agentpool/pool.go`, `engine.New`. Signature stayed fixed, so no caller changed.

**Total**: ~25 lines of product + test change (1 product file, 1 new test file, 3 test edits) + this doc. No new product fields, no signature changes, no new product files.

### Out of scope (deferred to B4)

- Per-turn tier selection in engine main loop (currently always `TierFast`); planner output → tier mapping ships in B4 alongside progressive disclosure
- `cmd/cli.go:349` planner `llm.NewClient` migration — B4 territory per ADR-002:115 because planner-on-flash is gated on ADR-004 progressive disclosure landing first (memory `priortext-avalanche-invalidates-planner`)
- `TraceRecord.TaskTier` populator wiring (still schema-only slot)
- Adding `SharedDeps.Router` field — B4 adds it together with its first reader and the matching `sharedDepConcreteTypes` update

### Risks

- **Eval test model sweep**: golden_test + evaluate_test iterate over multiple models; each `t.Run` builds a fresh Router with that model as base. Skip semantics differ (preserved, not unified): `golden_test.go:681` `t.Skipf`; `evaluate_test.go:175-176` `t.Logf + continue`. Both fire before the Router build.
- **Router build count after B2a**: build-inside replaces the `NewClient` call inside `NewSharedDeps` with a `NewRouter` call — same number of constructions as before (one client → one base-only router yielding one client), just via the factory. The HTTP grounded-renderer path still builds its own router in `applySharedDepsFromEnv` (B1, untouched). No `SharedDeps.Router` field, so nothing is stored/shared; B4 consolidates.
- **Empty-model fail-loud (B-7)**: `NewSharedDeps` now errors when `cfg.Agent.LLM.Model == ""` (via `NewRouter`). Production `config.Load` always populates a model; the only affected fixture was `agentpool` `minimalConfig` (fixed). Pinned by `TestNewSharedDeps_EmptyBaseModel_ReturnsError`.
- **Test mock compatibility**: 12 `LLMClient: &mockLLM{}` / `<var>` sites under `internal/engine/*_test.go` rely on the field being the `LLMClient` interface. Build-inside does NOT change the field type → all keep compiling. Full `go test ./...` is the gate (verified green).
- **Capability tests** consuming `SharedDeps.LLMClient` keep working — field type + population path unchanged; B2a only swaps the upstream factory call (direct `NewClient` → Router factory pinned to TierFast = base).

### Verification

```bash
# Build + unit tests (includes leak audit) — DONE, green
go build ./... && go test ./... -count=1

# Grep invariant — products only, exclude OCR (ADR-002:82) and tests — DONE
grep -rnE "llm\.NewClient\(" cmd/ internal/ eval/ \
    --include="*.go" --exclude="*_test.go" --exclude-dir="ocr"
# Expected EXACTLY 1 hit (B-9):
#   cmd/cli.go:349   (planner — B4-deferred per ADR-002 Risks #3)
# router.go:82's bare in-package NewClient( is the factory source (not llm.-qualified).
# B4 removes cli.go:349 → 0. Anything else = B2a acceptance gap.

# CLI smoke 3 questions (pending — run before merge)
./agent.exe cli -c deploy/conf/agent.yaml
# Q1 "我有哪些实例"  → resource_info, tools as before
# Q2 a RAG-covered FAQ (e.g. "实例怎么开机" / "GPU 监控在哪看") → knowledge/capability path
#    NB: avoid "怎么用 SecurityToken 签名" — confirmed corpus gap (0 chunks), not a routing signal
# Q3 "4090 多少钱"  → pricing_query capability
# Pass = intents + tool selections match main HEAD (zero behavior change).
```

---

## B2b: Skill/Tool Directory Split + Codegen (outline only — separate spec after B2a)

**Drafted now for visibility, fleshed out as a standalone plan doc after B2a ships.**

### Goal

Implement ADR-003 Acceptance:

- Build `internal/skills/<skill>/skill.md` directory + `internal/skills/loader.go`
- Build `internal/tools/<tool>.json` spec format with `x-compshare` extension
- Migrate 6 active `internal/intent/capabilities/*.md` → split each into `(skill metadata, skill body, tool spec)` triple
- Replace `internal/intent/capability_registry.go` Go literal array with `go generate ./...` codegen from directory scan
- Progressive disclosure: planner system prompt embeds only skill metadata, body loaded on trigger

### Double-run safety (per blueprint "B2 双跑" hint)

Keep the old `capability_registry.go` array alive as `legacyCapabilityRegistry` behind a env flag `USE_SKILL_LOADER` (default off). New `skills/` codegen produces a parallel `skillRegistry`. Dispatcher reads from whichever flag selects; trace records which path served the turn.

Cutover protocol:
1. PR ships skill loader OFF by default, codegen + legacy both build
2. N=20 regression in trace-only mode comparing two paths side-by-side (deterministic dispatch should produce identical handler results)
3. Flag flip to ON, legacy registry kept for 2 weeks as instant rollback
4. Legacy registry deleted in B2b-followup PR

### Risk

- Prompt content shifts byte-for-byte if frontmatter formatting differs from current. `systemPromptSHA256Baseline` will bump — needs explicit per-skill justification. Tracking via planner_examples_test.go same as B1 disk-info.
- planner few-shot may live in skill frontmatter — migrating means SHA bumps in waves. Defer per-capability ordering decision to B2b draft.

### Out of scope

- New capabilities — only the 6 active migrated. `IntentDiskInfo` (no handler) stays as planner-only.
- Planner output schema change — that's B4

---

## Open questions before B2a starts

**None — implemented.** Resolutions:

- ~~Option A vs Option B for SharedDeps~~ → **build-inside, signature unchanged** (rev-1 rejected Option A field; rev-2 chose signature-change "Option B"; rev-2-review **B-6** found that premise false and switched to build-inside)
- ~~Where Router is constructed~~ → **inside `NewSharedDeps`** via `llm.NewRouter(cfg.Agent.LLM, nil)` (transient, base-only). `cmd/shared_deps.go::buildLLMRouter` (B1 grounded-renderer router) stays as-is and untouched.

## Refs

- ADR-002 Acceptance #3 (B2 batch wording)
- ADR-002 Risks #3 (planner-on-flash dependency on ADR-004 — gates `cmd/cli.go:349`)
- ADR-003 Acceptance (skill/tool dir + codegen — B2b)
- blueprint `2026-05-28` — B2 batch breakdown (3 weeks, 双跑)
- memory:phase-a-strict-zero-behavior — B2a is structural plumbing, zero prompt drift
- memory:cli-regression-for-capability-changes — N≥3 CLI smoke required even though zero behavior change
- memory:priortext-avalanche-invalidates-planner — why `cmd/cli.go:349` is B4 not B2
- memory:reviewer-fact-check-before-apply — round 1 review verified line-by-line before applying
- memory:acceptance-invariant-with-allowed-change — round 1 finding B-1 (grep gate must list deferred sites)
- memory:shared-struct-setter-leaks-across-sessions — round 1 finding B-3 (leak audit drift if SharedDeps grows)
