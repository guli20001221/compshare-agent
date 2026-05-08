---
status: draft for review
parent: stage2a-phase0-tickets.md
ticket: T-005
phase: Phase 0
created: 2026-05-09
---

# T-005 RateLimiter / QuotaManager Contract

## Goal

Add a process-local RateLimiter / QuotaManager that gates LLM calls and mutating tool calls by a stable, non-secret subject key. It must provide the explicit quota hook required before T-007b can enable live shadow planner LLM calls.

T-005 is a safety and observability ticket. It must be deterministic, testable with a fake clock, and safe-by-default when configuration is absent.

## Non-Goals

- No Redis or cross-process distributed quota.
- No billing-grade accounting.
- No user identity system beyond the current per-API-key equivalent.
- No planner shadow routing, dashboard, or IntentPlan promotion.
- No mutating tool behavior changes beyond returning a friendly rate-limit denial before the call.
- No trace schema bump unless explicitly called out below.
- No persistence across process restarts. Daily counters reset on restart in Phase 0.

## Dependencies

Already satisfied:

- T-002 SecretBoundary, for the rule that raw API keys must never enter trace/audit/logs.
- T-001 SafeToolExecutor, for mutating tool policy classification.
- T-006 trace writer, for per-turn observability.
- T-007b commit 1/2, which need this quota hook before live planner LLM calls can be enabled.

## Core Decisions

### 0. Ownership and Injection

Choose Engine-owned limiter with narrow hook injection.

- `Engine.New()` constructs a process-local `governance.RateLimiter` from resolved config and stores it on `Engine`.
- `SafeToolExecutor` receives the limiter through a constructor option, such as `tools.WithRateLimiter(limiter)`, and never reaches back into `Engine`.
- `ShadowRunner` receives a quota hook through `ShadowRunnerOptions`, for example `QuotaHook func(governance.Request) governance.Decision`. If package boundaries make importing `internal/governance` from `internal/intent` undesirable, use an equivalent narrow interface in `intent` plus an adapter in engine/CLI wiring.
- CLI code should not create a separate limiter. It passes config to `Engine.New()` and observes decisions through trace recorder setters.

This keeps one process-local quota state per Engine instance and avoids package-level singletons.

### 1. Subject Key

Use the current CompShare public key as the Phase 0 subject identity.

Rules:

- The limiter receives only a hashed subject key, never the raw public key.
- The hash format is full SHA-256 hex with the `sha256:` prefix.
- The hash input is the exact public key string after config/env resolution.
- If the public key is missing, use subject key `anonymous` and emit a stderr warning at startup. Do not synthesize a fake hash from an empty string.
- The raw public key, private key, LLM API key, and ProjectId must never appear in limiter errors, trace fields, test artifacts, or logs.

Implementation hint:

- Add a small helper in `internal/governance`, such as `SubjectKeyFromPublicKey(publicKey string) (string, bool)`.
- It may use `observability.HashTracePayload(publicKey)` or a local SHA-256 helper, but it must not call `fmt.Sprintf("%v", cfg)` or hash the whole config struct.

### 2. Quota Classes

T-005 defines two quota classes:

| Class | Applies to | Default QPS | Default daily |
| --- | --- | ---: | ---: |
| `llm` | Main ReAct LLM calls and T-007b shadow planner LLM calls | 5 | 5000 |
| `mutating_tool` | SafeToolExecutor actions with `ActionClassMutating` or `ActionClassDestructive` | 1 | 50 |

Read-only tool calls are not rate-limited in T-005. They remain protected by SafeToolExecutor retry policy and upstream API limits.

### 3. Enforcement Semantics

Every limited call asks the limiter before executing:

```go
decision := limiter.Allow(governance.Request{
    SubjectKey: subjectKey,
    Class:      governance.ClassLLM,
    Action:     "main_react_chat",
    Now:        now,
})
```

Decision rules:

- If allowed, proceed with the call and record an allow decision for observability.
- If denied, do not call the underlying LLM/tool.
- Deny returns `ErrRateLimited`.
- The caller translates `ErrRateLimited` into a friendly user-visible message where applicable.
- Shadow planner denial must not fail the user request; it writes a disabled or invalid planner trace state and skips the planner LLM call.
- Main ReAct LLM denial may return a friendly assistant message because no safe answer can be generated.
- Mutating tool denial must return before calling the mutating API.
- For L1 actions, quota check must happen before the confirmation prompt. Denied requests return the canned quota message and must not ask the user to confirm.

### 4. Token Bucket + Daily Counter

The limiter is process-local and in-memory.

Per `(subject_key, class)` state:

- QPS bucket:
  - capacity = configured QPS
  - refill rate = configured QPS tokens per second
  - cost per request = 1 token
- Daily counter:
  - local date boundary based on injected clock location
  - cost per request = 1
  - reset when date changes

Ordering:

1. Check daily counter first.
2. Check QPS bucket second.
3. Only after both checks pass should either counter be consumed.

This avoids consuming a QPS token for a request that is already over daily quota.

`retry_after_ms`:

- For `qps_exceeded`, use the time until the next token would be available.
- For `daily_exceeded`, use the time until the next local-date boundary in the injected clock location.

### 5. Configuration

Default limits come from baseline §3.9.4:

> Per-API-key limits: LLM QPS 5, daily LLM calls 5000, mutating tool QPS 1, daily mutating calls 50.

```yaml
rate_limit:
  llm_qps: 5
  llm_daily: 5000
  mutating_qps: 1
  mutating_daily: 50
```

Implementation options:

- Preferred for Phase 0: add optional fields to the existing loaded config struct under `agent.rate_limit`.
- Do not introduce a separate required `internal/config/governance.yaml` file; optional config is enough and avoids another startup path.
- Missing or zero values use defaults.
- Negative values are invalid and should fail config load with a clear error.

### 6. Trace / Audit Semantics

T-006 trace.v0.1 has no dedicated `rate_limit` object. T-005 may add an additive optional block without changing existing fields:

```json
"rate_limit": {
  "checked": true,
  "allowed": false,
  "class": "llm",
  "action": "shadow_planner",
  "reason": "qps_exceeded",
  "subject_hash": "sha256:...",
  "retry_after_ms": 200
}
```

Rules:

- The block is optional and defaults to zero values when no limit was checked.
- Phase 0 does not add a top-level turn subject identity. Subject identity is observable only through this rate-limit block when a check occurs. A future trace schema may add top-level `subject_hash` if cross-turn subject analysis becomes necessary.
- The subject field is a hash only.
- `reason` enum values: `""`, `qps_exceeded`, `daily_exceeded`.
- For multiple checks in one user turn, record the first denial if any; otherwise record the last allow decision. T-007b dashboard only needs to know whether quota blocked the planner call.
- Trace writing failure must not affect rate-limit behavior.

If adding this block creates too much scope in implementation, T-005 commit 1 may ship the limiter without trace wiring, but commit 3 must wire it before T-007b live shadow calls are enabled.

### 7. Integration Points

LLM calls:

- Main ReAct path: check `ClassLLM` immediately before `e.llmClient.Chat`.
- T-007b shadow planner: check `ClassLLM` immediately before `CompleteIntentPlan`.
- Denied shadow planner calls are trace-only denials and do not fail the user request.

Mutating tools:

- Check `ClassMutatingTool` for SafeToolExecutor policies where `Class` is `ActionClassMutating` or `ActionClassDestructive`.
- Do not rate-limit read-only actions in T-005.
- Do not check workflow internal steps twice. Rate-limit the user-visible workflow action once at the top-level workflow action, not each internal API step.
- L2 destructive actions are already blocked by SafeToolExecutor. If a destructive action would be blocked before execution, it does not need to consume quota.

### 8. Friendly Messages

Use fixed, canned messages:

- QPS quota:
  - `请求过于频繁，请稍后再试。`
- Daily quota:
  - `今日额度已用完，请明天再试。`

Do not include quota numbers, subject hashes, API keys, or provider error bodies in the user-facing message.

## Implementation Plan

### Commit 1: In-Memory Limiter Core

Files:

- Create: `internal/governance/ratelimit.go`
- Test: `internal/governance/ratelimit_test.go`

Scope:

- Define `RateLimiter` as an interface with `Allow(Request) Decision`.
- Implement `MemoryLimiter` as the Phase 0 in-memory implementation.
- Implement quota classes, request/decision types, `ErrRateLimited`, defaults, token bucket, daily counter, and fake-clock friendly options.
- Implement subject-key hashing helper.
- No engine/CLI/trace integration.

Acceptance:

- Compile-time assertion: `var _ RateLimiter = (*MemoryLimiter)(nil)`.
- Unit test: QPS limit allows first `N` calls and denies `N+1` at same timestamp.
- Unit test: QPS bucket refills with fake clock.
- Unit test: daily quota denies after configured daily limit.
- Unit test: different subject hashes count independently.
- Unit test: LLM and mutating classes count independently.
- Unit test: missing public key returns `anonymous` plus `ok=false`; raw empty string is not hashed.
- Unit test: raw public key never appears in decision/error strings.
- `go test ./internal/governance -count=1` passes.

### Commit 2: Config Loading

Files:

- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

Scope:

- Add optional `agent.rate_limit` config fields.
- Apply defaults when fields are omitted or zero.
- Reject negative values with clear errors.
- Do not require a new config file.

Acceptance:

- Unit test: omitted `rate_limit` yields defaults.
- Unit test: partial overrides merge with defaults.
- Unit test: negative qps/daily values fail config load.
- Existing config tests pass.

### Commit 3: Trace / Audit Wiring

Files:

- Modify: `internal/observability/trace.go`
- Modify: `cmd/trace.go`
- Test: `internal/observability/trace_test.go`
- Test: `cmd/trace_test.go`

Scope:

- Add optional `rate_limit` block to trace.v0.1.
- Add trace-recorder setter for rate-limit decisions.
- Ensure decision trace contains only hashed subject key.
- Do not enforce anything yet.

Acceptance:

- Unit test: default trace contains zero-value rate limit block or omits it consistently.
- Unit test: denied decision writes `checked=true`, `allowed=false`, class/action/reason/retry_after_ms.
- Unit test: trace line does not contain raw public key.
- Unit test: when multiple decisions occur, first denial wins; if no denial, last allow wins.
- Unit test: simulated trace write failure does not change allow/deny behavior.

### Commit 4: Engine LLM Hook

Files:

- Modify: `internal/engine/engine.go`
- Modify: `cmd/agent.go`
- Test: `internal/engine/engine_test.go`
- Test: `cmd/trace_test.go` if trace decision propagation is in CLI recorder

Scope:

- Construct limiter from config.
- Compute subject hash from resolved public key.
- Check `ClassLLM` before main ReAct `llmClient.Chat`.
- Return the canned LLM quota message on denial.
- Preserve behavior when limiter is nil or config uses defaults.

Acceptance:

- Unit test: denied LLM request does not call mock LLM.
- Unit test: denied LLM request returns friendly canned message.
- Unit test: daily denial returns the daily canned message, not the QPS canned message.
- Unit test: allowed LLM request preserves existing behavior.
- Unit test: trace records rate-limit denial without raw subject.
- `go test ./internal/engine ./cmd -count=1` passes.

### Commit 5: Mutating Tool Hook

Files:

- Modify: `internal/tools/safe_executor.go` or `internal/engine/engine.go` depending on the narrowest hook
- Test: `internal/tools/safe_executor_test.go` and/or `internal/engine/engine_test.go`

Scope:

- Check mutating quota once for user-visible mutating actions.
- Avoid double-counting workflow internal steps.
- Return canned mutating quota message before the API call.
- Do not consume quota for L2 actions blocked before execution.

Acceptance:

- Unit test: denied mutating action does not call inner executor.
- Unit test: denied L1 action does not invoke the confirm callback.
- Unit test: workflow internal steps are not double-counted.
- Unit test: read-only actions are not rate-limited.
- Unit test: L2 blocked actions do not consume quota.

### Commit 6: T-007b Planner Quota Hook

Files:

- Modify: `internal/intent/shadow.go`
- Test: `internal/intent/shadow_test.go`

Scope:

- Add a quota hook to `ShadowRunner` before `planner.Plan`.
- Denied quota skips planner call.
- Denial returns a planner trace that is safe for production flow and can be written to trace.
- This commit enables T-007b commit 3 engine/CLI shadow wiring to call a quota-protected runner.

Acceptance:

- Unit test: quota denial skips planner and returns `enabled=true`, `schema_valid=false`, `intent=unknown`.
- Unit test: quota denial preserves no raw subject/key in trace.
- Unit test: quota allow calls planner once.
- Unit test: disabled mode still does not call quota hook or planner.

### Commit 7: Smoke Artifact

Files:

- Add: `eval/capability/2026-05-09-t005-rate-limiter-smoke.md`

Scope:

- Run a local/mock smoke or tightly bounded real-account smoke that triggers QPS denial without provider spam.
- Prefer mock/in-process smoke for mutating quota. Do not fire real mutating APIs.
- If real LLM smoke is used, keep it to the minimum needed to prove friendly denial.

Acceptance:

- Artifact records command shape, limits used, denial result, trace rate-limit block, and secret scan result.
- Artifact contains no raw public/private keys, LLM API key, ProjectId, UHostIds, IPs, raw user prompts, or provider response bodies.

## Review Checklist

- [ ] Limiter key is hashed; raw API keys never enter error/trace/log strings.
- [ ] Daily and QPS counters are checked atomically under lock.
- [ ] Daily rejection does not consume QPS tokens.
- [ ] Fake clock tests cover refill and date rollover.
- [ ] Shadow planner quota denial skips `CompleteIntentPlan`.
- [ ] Main LLM quota denial skips `llmClient.Chat`.
- [ ] Mutating quota denial skips the mutating API call.
- [ ] Workflow actions are not double-counted.
- [ ] Read-only actions remain unaffected.
- [ ] User-facing messages are fixed canned strings.

## Follow-Up After T-005

- T-007b commit 3 may wire live shadow planner calls only through the quota-protected runner.
- Phase 0 daily counters reset on process restart, so frequent restarts can bypass the daily cap. This is an accepted single-instance limitation until Stage 3 persistence.
- Stage 3 may replace the in-memory limiter with Redis-backed distributed quotas.
