# B2 — Router Inject Completion + Skill/Tool Dir Foundation

**Date**: 2026-05-29
**Driver**: ADR-002 Acceptance #3 (engine.go:296 + golden_test.go:684 + evaluate_test.go:180 inject point migration) + ADR-003 (skill/tool 目录拆) + blueprint B2 batch
**Audience**: Codex (实施) + Claude (reviewer)
**Estimated effort**: B2a 1-2 days / B2b 2-3 weeks
**Revision**: 2 (2026-05-29) — review round 1 fixed 5 findings: grep invariant self-contradiction (B-1), Option A pseudo-code type error (B-2), missing leak-audit cost (B-3), double-build wording (B-4), eval skip mechanism imprecision (B-5). Implementation path switched from Option A → Option B per reviewer-recommended YAGNI scope.

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

Migrate 2 of the 3 remaining product `llm.NewClient(cfg)` callsites (engine internal + 2 eval) to consume `Router.For(TierFast)`. After B2a ships, `grep llm.NewClient(` in product code (excluding `*_test.go`) matches exactly **3 callsites**:

| File:line | Status after B2a | Why still present |
|---|---|---|
| `internal/llm/router.go` | unchanged | Router internal — canonical single source factory |
| `internal/ocr/client.go:27` | unchanged | OCR — explicitly NOT migrated per ADR-002:82 (independent path) |
| `cmd/cli.go:349` | **unchanged in B2a** | Planner — B4-deferred per ADR-002 Risks #3 (planner-on-flash requires ADR-004 progressive disclosure landing first; otherwise priortext-avalanche per memory) |

Test files (`*_test.go`) under `eval/` also lose their direct `llm.NewClient(...)` calls — only `golden_test.go:684` and `evaluate_test.go:180` are in B2a scope; any other test sites are out of scope unless surfaced by the grep gate below.

CLI + HTTP behavior is byte-stable: empty `tier_routing` in config means all tiers point at the base `cfg.Agent.LLM` (already B1 invariant, ADR-002 Acceptance #5).

### Acceptance

- [ ] `internal/engine/engine.go:296` `llm.NewClient(cfg.Agent.LLM)` migration: see **Implementation: Option B** below — `LLMClient LLMClient` interface field UNCHANGED, only the source of the value changes. `NewSharedDeps` gains a `*llm.Router` parameter (signature change) supplied by callers
- [ ] `eval/golden_test.go:684` `llm.NewClient(config.LLMConfig{...})` → goes through a per-test Router. Each baseline model in the model sweep is wrapped in a `Router` with that model as `cfg.Agent.LLM` and `TierRouting=nil` — `Router.For(TierFast)` returns the same client byte-for-byte
- [ ] `eval/evaluate_test.go:180` same pattern as golden_test
- [ ] Product grep gate: `grep -nE "llm\.NewClient\(" cmd/ internal/ eval/ --include="*.go" --exclude="*_test.go" --exclude-dir="ocr"` returns exactly 2 lines (`router.go` Router-internal NewClient + `cmd/cli.go:349` B4-deferred planner). The `--exclude="*_test.go"` is required because golden/evaluate test files are still `.go` (skip-or-fail driven); the `--exclude-dir="ocr"` is required because ADR-002:82 says OCR is independent and stays. cli.go:349 is the only remaining product sentinel and removed in B4
- [ ] `go test ./... -count=1` green — in particular `TestSharedDeps_NoMutatingSetterLeakage` + `TestSharedDeps_AuditCoversAllSharedDepFields` (`internal/engine/shared_deps_leak_audit_test.go`) must stay green WITHOUT modification, because Option B does not add a new SharedDeps field
- [ ] CLI smoke N≥3 questions (1 fast / 1 knowledge / 1 mixed) — no regression vs main HEAD; trace JSONL `task_tier` field continues to be populated as B4-deferred (`""` / not yet wired), no schema break
- [ ] Backward-compat invariant pinned by a test: empty `tier_routing` → `engine.NewSharedDeps` produces an LLMClient pointing at the same `(BaseURL, Model, APIKey)` triple as the pre-B2a path. Test name: `TestNewSharedDeps_RouterDerivedClient_ByteStableWithEmptyTierRouting`

### Implementation: Option B (recommended)

Keep the `SharedDeps.LLMClient LLMClient` interface field **unchanged**. Only swap the source: pass an externally-built `*llm.Router` into `NewSharedDeps`, then assign `router.For(TierFast)` (which returns `*llm.Client` satisfying the existing `LLMClient` interface in `engine.go:107`) into the same field.

```go
// internal/engine/engine.go — signature change only
func NewSharedDeps(cfg *config.Config, router *llm.Router) (*SharedDeps, error) {
    if cfg == nil {
        return nil, errors.New("engine.NewSharedDeps: cfg is nil")
    }
    if router == nil {
        return nil, errors.New("engine.NewSharedDeps: router is nil")
    }
    cap := llm.LookupCapability(cfg.Agent.LLM.BaseURL, cfg.Agent.LLM.Model)
    return &SharedDeps{
        LLMClient: router.For(llm.TierFast),  // interface satisfied by *llm.Client
        // ... rest unchanged ...
    }, nil
}
```

Callers (`cmd/cli.go` + `cmd/shared_deps.go`) already build a Router via `buildLLMRouter` for the grounded renderer in B1 — they reuse that one and pass it in. No third Router build is introduced. The `LLMClient` interface field stays identical to today, so all 12 `LLMClient: &mockLLM{}` / `LLMClient: <var>` injection sites under `internal/engine/*_test.go` continue to compile and pass without edit.

**What this does NOT do**:
- Does NOT add a `Router` field to `SharedDeps` — no consumer for it until B4, so YAGNI (memory:phase-a-strict-zero-behavior + Rule 2 simplicity)
- Does NOT change `LLMClient` interface type — preserves test mock compatibility
- Does NOT introduce a third Router build — `NewSharedDeps` parameter receives the existing one

### Why not Option A (rejected — kept here as design rationale)

Initial draft proposed adding `Router *llm.Router` as a new SharedDeps field, plus aliasing `LLMClient` to `router.For(TierFast)`. Three concrete costs surfaced in review:

| Issue | Cost |
|---|---|
| **Field type mismatch** | The literal pseudo-code wrote `LLMClient *llm.Client` (concrete). The actual field is `LLMClient LLMClient` interface (engine.go:107+257). Following the pseudo-code literally would break ~12 test injection sites (`LLMClient: &mockLLM{}` × 11 in `engine_session_test.go` / `session_state_test.go` / `toolfact_writer_test.go` + 1 variable assignment at `engine_session_test.go:306`) |
| **Leak audit drift** | `internal/engine/shared_deps_leak_audit_test.go:91-98` enumerates `sharedDepConcreteTypes` as a deliberate single source of truth; `:164 TestSharedDeps_AuditCoversAllSharedDepFields` walks every SharedDeps field. Adding a new `Router` field without updating either `sharedDepConcreteTypes` (with `reflect.TypeOf((*llm.Router)(nil))`) OR `nonAuditableFields` triggers `t.Errorf` at line 199. Initial spec listed cost as "1 extra field, no behavior change" — missed two-place sync requirement |
| **Speculative wiring** | B4 is the first reader of per-turn tier selection. Adding the field now means it sits unused for the entire B4 implementation window. ADR-007 anti-pattern + Rule 2 simplicity say don't add speculative state. B4 can add the field + audit update + populator in one coherent change |

Option B sidesteps all three by changing only the *source* of `LLMClient`, not its type or its container.

### Files touched

| File | Change | Lines |
|---|---|---|
| `internal/engine/engine.go` | `NewSharedDeps` gains `router *llm.Router` parameter; nil-check + assign `router.For(TierFast)` into existing `LLMClient` field | ~6 add (signature + nil-check + 1-line body change) |
| `internal/engine/engine_test.go` (or a new `engine_router_byte_stable_test.go`) | `TestNewSharedDeps_RouterDerivedClient_ByteStableWithEmptyTierRouting` — builds two SharedDeps (one direct old `llm.NewClient`, one via `Router.For(TierFast)` with empty TierRouting) and asserts identical `(BaseURL, Model, APIKey)` triple | ~30 add |
| `eval/golden_test.go` | Per-model `llm.NewRouter` + `Router.For(TierFast)` instead of direct `NewClient`. **Skip is `t.Skipf("SKIP %s: no API key", m.Name)` at line 681 — preserve placement before Router build so missing-key skip still fires first** | ~8 change |
| `eval/evaluate_test.go` | Same pattern. **NB: this file's skip is `t.Logf("SKIP %s: no API key ...") + continue` at lines 175-176 (NOT `t.Skipf`); preserve the Logf+continue idiom — Router build sits inside the `t.Run` at line 179, only reached after the continue check** | ~8 change |
| `cmd/shared_deps.go` | `buildHTTPServerPool` builds Router once via existing `buildLLMRouter`, passes to `engine.NewSharedDeps(cfg, router)`. Same Router instance is also used for grounded renderer via `applySharedDepsFromEnv` (already B1 behavior) — no duplicate build | ~5 change |
| `cmd/cli.go` | CLI session bootstrap: same pattern — build Router once, pass to `NewSharedDeps`. Grounded renderer also reuses | ~5 change |

**Total**: ~60 lines change across 6 files. No new fields, no new files.

### Out of scope (deferred to B4)

- Per-turn tier selection in engine main loop (currently always `TierFast`); planner output → tier mapping ships in B4 alongside progressive disclosure
- `cmd/cli.go:349` planner `llm.NewClient` migration — B4 territory per ADR-002:115 because planner-on-flash is gated on ADR-004 progressive disclosure landing first (memory `priortext-avalanche-invalidates-planner`)
- `TraceRecord.TaskTier` populator wiring (still schema-only slot)
- Adding `SharedDeps.Router` field — B4 adds it together with its first reader and the matching `sharedDepConcreteTypes` update

### Risks

- **Eval test model sweep**: golden_test + evaluate_test iterate over multiple models. Each iteration must build a fresh Router with that model as base. Skip semantics differ between the two files (see Files-touched notes above): `golden_test.go:681` uses `t.Skipf` which stops the test cleanly; `evaluate_test.go:175-176` uses `t.Logf + continue` which moves to the next iteration. Both are skip-not-fail semantically — preserve each file's idiom rather than unify
- **Router build count after B2a**: cmd/shared_deps.go:34-38 explicitly documents "called once at process boot — cli.go and shared_deps.go each call it for their own path (CLI vs HTTP) ... building twice in different binary entry points is acceptable". B2a does NOT add a third build — `NewSharedDeps` consumes the existing per-binary Router via parameter. Trace `runtime` line shows tier wiring as before; no new build observable in trace
- **Test mock compatibility**: 12 `LLMClient: &mockLLM{}` / `LLMClient: <var>` sites under `internal/engine/*_test.go` rely on the field being `LLMClient` interface. Option B does NOT change the field type, so all 12 keep compiling. CI run is the gate — if any breaks, it indicates an unintended Option-A-style type change snuck in
- **Capability tests** that consume `SharedDeps.LLMClient` directly should keep working since the field type and population path are unchanged; B2a only swaps the upstream factory call

### Verification

```bash
# Build + unit tests (includes leak audit)
go build ./... && go test ./... -count=1

# Grep invariants — products only, exclude OCR (per ADR-002:82) and tests
grep -nRE "llm\.NewClient\(" cmd/ internal/ eval/ \
    --include="*.go" --exclude="*_test.go" --exclude-dir="ocr"
# Expected exactly 2 hits:
#   internal/llm/router.go:<N>  (Router internal factory)
#   cmd/cli.go:349              (planner — B4-deferred per ADR-002 Risks #3)
# B4 acceptance removes cli.go:349. Anything else = B2a acceptance gap.

# CLI smoke 3 questions
./agent.exe cli -c deploy/conf/agent.yaml
# Q1 "我有哪些实例" → resource_info, tools as before
# Q2 "怎么用 SecurityToken 签名" → knowledge_qa, RAG
# Q3 "4090 多少钱" → pricing_query capability

# Trace JSONL byte-stable check vs main HEAD (intent classifications unchanged)
diff <(grep '"intent"' eval/traces_b2a_smoke/agent-trace-*.jsonl | head -3) \
     <(grep '"intent"' eval/traces_main_baseline/agent-trace-*.jsonl | head -3)
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

**None blocking.** Round 1 review settled:

- ~~Option A vs Option B for SharedDeps~~ → **Option B** (round 1 review B-2/B-3 surfaced Option A cost; reviewer recommended YAGNI scope, agreed)
- ~~Where Router is constructed~~ → existing `buildLLMRouter` in `cmd/shared_deps.go` (B1) stays canonical; B2a passes the result into `NewSharedDeps` rather than building a third instance

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
