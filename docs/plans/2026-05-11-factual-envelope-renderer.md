# Factual Envelope Renderer Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build an opt-in hybrid answer path where API results are converted into a deterministic factual envelope, then optionally summarized by an LLM renderer with validation and deterministic fallback.

**Architecture:** Keep `SafeToolExecutor` as the generic policy/security gateway. Put factual envelope DTOs in a neutral `internal/envelope` package so `internal/intent`, `internal/diagnosis`, `internal/renderer`, and `internal/observability` can share the contract without domain packages depending on renderer internals. Keep ReAct as the default runtime path; grounded rendering only runs when both Phase 1 cutover and `USE_GROUNDED_RENDERER=llm` are enabled.

**Tech Stack:** Go, existing `internal/llm` Modelverse client, existing `internal/intent` handlers, existing `internal/observability` trace JSONL.

---

## Non-Goals

- Do not make Phase 1 cutover or deterministic handlers the default CLI behavior. Default remains `deepseek-v4-flash + ReAct`.
- Do not expand curated RAG, vector search, embeddings, or mixed API+KB in this plan.
- Do not turn EntityRegistry into an enforcement validator.
- Do not remove existing deterministic `RenderResourceSummary` / `RenderMonitorSummary`; they remain fallback renderers.
- Do not expose raw API payloads to the grounded renderer.
- Do not enable billing cutover in the first implementation PR. Billing gets facts/envelope extraction first, then a separate handler/routing PR after quota/failure semantics are reviewed.

## Runtime Gates

- `USE_INTENT_PLANNER_FOR` still controls which intents are eligible for cutover.
- Add `USE_GROUNDED_RENDERER=llm` to enable LLM summarization for eligible Phase 1 handler results.
- If unset or invalid, Phase 1 cutover continues to use existing deterministic replies.
- The renderer uses the same configured model/client as the agent. A separate renderer model is out of scope for this slice.

## Envelope Contract

Create `internal/envelope` as the stable cross-package contract.

The contract must distinguish source facts from computed facts and must carry enough semantics for audit:

```go
type Kind string

const (
    KindResourceInfo    Kind = "resource_info"
    KindMonitorQuery    Kind = "monitor_query"
    KindBillingInstance Kind = "billing_instance"
)

type Envelope struct {
    Kind          Kind          `json:"kind"`
    SourceActions []string      `json:"source_actions"`
    Subjects      []Subject     `json:"subjects"`
    Facts         []Fact        `json:"facts"`
    Computed      []Fact        `json:"computed"`
    Constraints   Constraints   `json:"constraints"`
}

type Subject struct {
    ID    string `json:"id"`
    Name  string `json:"name,omitempty"`
    Type  string `json:"type"`
}

type Fact struct {
    SubjectID   string `json:"subject_id,omitempty"`
    Key         string `json:"key"`
    Label       string `json:"label"`
    Value       any    `json:"value"`
    Unit        string `json:"unit,omitempty"`
    Source      string `json:"source"`      // "api" | "computed"
    Period      string `json:"period,omitempty"` // "latest" | "hour" | "day" | "month"
    WindowStart int64  `json:"window_start,omitempty"`
    WindowEnd   int64  `json:"window_end,omitempty"`
    Aggregation string `json:"aggregation,omitempty"` // "latest" initially
}

type Constraints struct {
    DoNotInventInstances    bool `json:"do_not_invent_instances"`
    DoNotInventMetrics      bool `json:"do_not_invent_metrics"`
    DoNotAnswerAccountBill  bool `json:"do_not_answer_account_bill"`
}
```

Required helpers:

- `Hash(env Envelope) (string, error)` using `observability.HashTracePayload`.
- `AllowedIDs(env Envelope) map[string]struct{}` for renderer validation.
- `AllowedNames(env Envelope) map[string]struct{}` for conservative validation of obvious invented instance names.

## Commit Plan

### Commit 1: Envelope Contract and Resource/Monitor Builders

**Files:**
- Create: `internal/envelope/envelope.go`
- Create: `internal/intent/envelope.go`
- Modify: `internal/intent/handler.go`
- Test: `internal/envelope/envelope_test.go`
- Test: `internal/intent/envelope_test.go`
- Test: existing `internal/intent/handler_*_test.go`

**Step 1: Write failing envelope tests**

Add tests that assert:

- Resource envelope is stable, sorted by `UHostId`, and hashes consistently.
- Resource envelope includes only customer-safe fields from `entity.InstanceSnapshot`: `UHostId`, `Name`, `State`, `OsType`, `GPU`, `GpuType`, `ImageType`, `StartTime`, `CPU`, `Memory`, `Zone`, `Region`, `ChargeType`, `ExpireTime`, `AutoRenew`.
- Monitor envelope takes resolved subjects explicitly: `BuildMonitorEnvelope(subjects []entity.InstanceSnapshot, metrics []Metric, payload map[string]any)`.
- Monitor envelope uses the resolved target snapshots/IDs from `HandleMonitorQuery`, not IDs guessed from the monitor payload. Tests must assert `Envelope.Subjects`, `AllowedIDs`, and hash stability for a monitor query.
- Monitor envelope uses the current demo parser reality: it redacts first, flattens scalar fields with `flattenScalars`, filters with `matchesRequestedMetric`, and records selected values as `aggregation="latest"` unless explicit timestamps are found.
- Monitor envelope does not claim max/average semantics.
- Resource and monitor builders set constraints explicitly:
  - resource: `DoNotInventInstances=true`, `DoNotAnswerAccountBill=true`;
  - monitor: `DoNotInventInstances=true`, `DoNotInventMetrics=true`, `DoNotAnswerAccountBill=true`.
- Existing deterministic replies are unchanged.

Run:

```powershell
go test ./internal/envelope ./internal/intent -run Envelope -count=1
```

Expected: FAIL because packages/builders do not exist.

**Step 2: Add `internal/envelope`**

Add the contract above plus hash/allowed helper tests.

Do not import `internal/renderer` here.

**Step 3: Add builders in `internal/intent/envelope.go`**

Implement:

- `BuildResourceEnvelope(instances []entity.InstanceSnapshot) envelope.Envelope`
- `BuildMonitorEnvelope(subjects []entity.InstanceSnapshot, metrics []Metric, payload map[string]any) envelope.Envelope`

Use existing `handler.go` helpers as they exist today:

- `flattenScalars`
- `matchesRequestedMetric`
- `safeValue`

If a helper must be extracted/renamed, do that explicitly in this commit and keep existing renderer tests green.

**Step 4: Attach envelope to handler result without changing behavior**

Extend `HandlerResult`:

```go
Envelope *envelope.Envelope
RendererInputEnvelopeHashes []string
```

In `HandleResourceInfo` and `HandleMonitorQuery`, set `Envelope` and `RendererInputEnvelopeHashes`, but keep `Reply` as the existing deterministic output.

For monitor, preserve the resolved instance snapshots from `resolveResourceTargets` or add a sibling helper that returns snapshots, not just IDs. Do not reconstruct monitor subjects from API payload.

**Step 5: Verify**

```powershell
go test ./internal/envelope ./internal/intent ./internal/observability -count=1
```

Expected: PASS.

**Step 6: Commit**

```powershell
git add internal/envelope internal/intent
git commit -m "feat: add factual envelopes for resource and monitor handlers"
```

### Commit 2: Grounded Renderer Core and Validation

**Files:**
- Create: `internal/renderer/renderer.go`
- Create: `internal/renderer/validator.go`
- Create: `internal/renderer/prompt.go`
- Test: `internal/renderer/renderer_test.go`
- Test: `internal/renderer/validator_test.go`

**Step 1: Write failing renderer tests**

Tests:

- Renderer sends only envelope JSON plus renderer instructions, not raw API payload.
- Renderer returns model text when LLM output passes validation.
- Renderer rejects unknown `uhost-...` identifiers not present in the envelope.
- Renderer rejects obvious unknown instance names when they are not in `AllowedNames(env)`. Keep the rule conservative: only reject names matching known instance-name-like tokens in tests, not arbitrary Chinese nouns.
- Renderer rejects account-level bill/balance/transaction claims when `DoNotAnswerAccountBill=true`.
- Renderer rejects numeric percent claims for monitor envelopes with no metric facts.
- Renderer returns a typed fallback reason when LLM call or validation fails.

Run:

```powershell
go test ./internal/renderer -count=1
```

Expected: FAIL.

**Step 2: Add renderer interface**

In `internal/renderer/renderer.go`:

```go
type LLMClient interface {
    Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error)
}

type Renderer interface {
    Render(ctx context.Context, req RenderRequest) RenderResult
}

type RenderRequest struct {
    Envelope envelope.Envelope
    Fallback string
    Model    string
}

type RenderResult struct {
    Text            string
    Model           string
    LatencyMS       int64
    AttributionMode string // "envelope"
    EnvelopeHash    string
    FallbackUsed    bool
    FallbackReason  string // "" | "llm_error" | "validation_failed" | "rate_limited"
}
```

`Render` returns a result, not `(result, error)`, so fallback status is explicit and traceable.

**Step 3: Add grounded renderer**

Implement `GroundedRenderer`:

- Uses `llm.ChatRequest`.
- Uses no tools.
- System prompt requires:
  - answer only from envelope facts,
  - prefer concise tables for multiple subjects,
  - never invent instances, metrics, prices, dates, or account-level billing facts,
  - say what is missing if facts are insufficient.
- On LLM error or validation error, returns `FallbackUsed=true` and `Text=Fallback`.

**Step 4: Validation**

Implement `ValidateRenderedText(env envelope.Envelope, text string) error`.

Minimum validation:

- Find `uhost-[A-Za-z0-9_-]+` tokens; every token must exist in `env.Subjects`.
- For resource/monitor envelopes, reject clearly instance-like names from the output when they are neither an allowed envelope name nor a generic label. Tests must cover one invented English/ASCII instance name.
- If `DoNotAnswerAccountBill`, reject account balance / monthly total / transaction flow claims.
- If `KindMonitorQuery` and envelope has no metric facts, reject percent claims like `12%`.
- Keep validation conservative; false positive falls back to deterministic reply.

**Step 5: Verify**

```powershell
go test ./internal/renderer -count=1
```

Expected: PASS.

**Step 6: Commit**

```powershell
git add internal/renderer
git commit -m "feat: add grounded LLM renderer"
```

### Commit 3: Engine, CLI, and Trace Wiring for Resource/Monitor Renderer

**Files:**
- Modify: `internal/engine/engine.go`
- Modify: `internal/engine/engine_test.go`
- Modify: `cmd/agent.go`
- Modify: `cmd/trace.go`
- Modify: `cmd/trace_test.go`
- Modify: `internal/observability/trace.go`
- Modify: `internal/observability/trace_test.go`

**Step 1: Write failing engine tests**

Tests:

- With Phase 1 cutover enabled and grounded renderer installed, `resource_info` returns renderer text.
- Renderer fallback returns deterministic reply and does not re-enter ReAct.
- Renderer LLM call consumes `governance.ClassLLM` with action `grounded_renderer`.
- Renderer rate-limit denial produces deterministic fallback and trace `fallback_reason="rate_limited"`.
- When renderer is disabled, existing deterministic behavior is unchanged.

Run:

```powershell
go test ./internal/engine -run GroundedRenderer -count=1
```

Expected: FAIL.

**Step 2: Add engine field and setter**

In `Engine`:

```go
groundedRenderer renderer.Renderer
groundedRendererModel string
rendererTraceObserver func(observability.RendererTrace)
```

Add:

```go
func (e *Engine) SetGroundedRenderer(r renderer.Renderer, model string)
func (e *Engine) SetRendererTraceObserver(fn func(observability.RendererTrace))
```

**Step 3: Wire `tryPhase1Cutover`**

After handler returns `HandlerStatusHandled`:

1. If `handled.Envelope == nil` or renderer nil, use existing deterministic reply.
2. Check `allowRateLimited(governance.ClassLLM, "grounded_renderer")`.
3. If denied, use deterministic reply and emit renderer trace with `fallback_reason="rate_limited"`.
4. If allowed, call renderer with envelope and deterministic fallback.
5. Append final assistant message exactly once.

**Step 4: Trace fields**

Do not bump schema only for additive fields unless tests require it. Keep `trace.v0.2` if readers stay compatible.

Extend `observability.RendererTrace`:

```go
Enabled bool `json:"enabled"`
Status string `json:"status"` // "" | "rendered" | "fallback"
EnvelopeKind string `json:"envelope_kind"`
InputEnvelopeHashes []string `json:"input_envelope_hashes"`
FallbackUsed bool `json:"fallback_used"`
FallbackReason string `json:"fallback_reason"`
```

Keep existing fields:

- `Model`
- `LatencyMS`
- `AttributionMode`
- `InputToolCallIDs`
- `InputToolArgHashes`

`cmd/trace.go` must have a renderer trace setter. Tests must prove fields are actually populated, not just present as zero values.

`cmd/agent.go` must reset and wire the observer per turn:

```go
eng.SetRendererTraceObserver(nil)
...
if traceEnabled {
    traceRecorder := newCLITraceRecorder(...)
    eng.SetRendererTraceObserver(traceRecorder.SetRendererTrace)
}
```

Add a `cmd` test that simulates a renderer trace through the recorder and asserts `renderer.enabled/status/input_envelope_hashes/fallback_reason` appear in the JSONL. If a higher-level CLI test exists for per-turn reset, extend it so a previous turn's renderer trace cannot leak into the next line.

**Step 5: CLI env gate**

In `cmd/agent.go`, add:

- `USE_GROUNDED_RENDERER=llm` enables `renderer.NewGroundedRenderer(llm.NewClient(cfg.Agent.LLM))`.
- Unknown values print a warning and disable grounded renderer.
- Startup banner prints renderer mode alongside planner mode.

**Step 6: Verify**

```powershell
go test ./internal/engine ./cmd ./internal/observability -count=1
```

Expected: PASS.

**Step 7: Commit**

```powershell
git add internal/engine cmd internal/observability
git commit -m "feat: wire grounded renderer into phase1 cutover"
```

### Commit 4: Billing Facts Extraction Only

**Files:**
- Modify: `internal/diagnosis/billing_anomaly.go`
- Modify: `internal/diagnosis/billing_anomaly_test.go`
- Create: `internal/diagnosis/billing_facts_test.go`

**Scope:** Do not enable billing cutover in this commit. This only extracts reusable billing facts so a later PR can build `billing_instance` envelope/handler with smaller risk.

**Step 1: Write failing billing facts tests**

Tests:

- Running Dynamic/Postpay/Spot actual compute charge uses `InstancePrice`.
- Stopped Dynamic/Postpay/Spot actual compute charge is zero, but disk/image charges remain.
- Day/Month prepaid facts preserve prepaid charge type and do not pretend stopped equals free.
- Mixed all-instance facts compute totals and stopped retained cost consistently with current `buildBillingSummary`.

Run:

```powershell
go test ./internal/diagnosis -run BillingFacts -count=1
```

Expected: FAIL.

**Step 2: Extract facts helper**

Add:

```go
type BillingInstanceFact struct { ... }
type BillingFactsSummary struct { ... }
func BuildBillingFacts(hosts []any) BillingFactsSummary
```

Use concrete fields, not string-only summaries:

```go
type BillingInstanceFact struct {
    UHostID string
    Name string
    State string
    ChargeType string
    GpuType string
    GPU int
    InstancePrice float64
    DiskPrice float64
    ImagePrice float64
    ActualComputeCharge float64
    RetainedStoppedCharge float64
    Period string // "hour" | "day" | "month" | "year" | "unknown"
}

type BillingFactsSummary struct {
    Instances []BillingInstanceFact
    HourlyTotal float64
    StoppedRetainedTotal float64
    RunningCount int
    StoppedCount int
    HasDynamic bool
    HasPrepaid bool
}
```

Make existing `buildBillingSummary` call `BuildBillingFacts` so old `DiagnoseBilling` output remains unchanged.

**Step 3: Verify**

```powershell
go test ./internal/diagnosis ./internal/engine -count=1
```

Expected: PASS, including `TestDiagnoseBillingConsumesMultipleReadExpensiveQuotaUnits`.

**Step 4: Commit**

```powershell
git add internal/diagnosis
git commit -m "refactor: extract billing facts for future envelopes"
```

### Commit 5: Real-Account Smoke Artifact

**Files:**
- Add: `eval/capability/2026-05-11-factual-envelope-renderer-smoke.md`

**Step 1: Run unit verification**

```powershell
go test ./... -count=1
python scripts/test_planner_vs_guard_diff.py
powershell -ExecutionPolicy Bypass -File scripts/secret_scan.ps1
git diff --check
```

Expected: PASS / no output for secret scan and diff check.

**Step 2: Run real-account smoke**

Use `deepseek-v4-flash`, trace enabled, planner cutover opt-in, and grounded renderer enabled:

```powershell
$env:USE_INTENT_PLANNER="shadow"
$env:USE_INTENT_PLANNER_FOR="resource,monitor"
$env:USE_GROUNDED_RENDERER="llm"
$env:COMPSHARE_TRACE_ENABLED="1"
```

Smoke questions:

1. One resource info single-instance question.
2. One resource info all-instance question.
3. One current monitor single-instance question.
4. One all-running monitor question.
5. One account-level bill/balance/transaction question.

Billing instance stays ReAct in this PR; include only as an observation if manually tested, not as acceptance.

**Step 3: Artifact rules**

The artifact must include:

- model and runtime env,
- number of turns,
- cutover/renderer status per turn,
- trace schema version,
- renderer fallback count and reasons,
- factual correctness summary against GT,
- hard-block confirmation for account billing,
- no raw trace, raw transcript, API keys, IPs, ProjectId, UHostIds, or billing amounts beyond sanitized summaries.

**Step 4: Commit**

```powershell
git add eval/capability/2026-05-11-factual-envelope-renderer-smoke.md
git commit -m "test: add factual envelope renderer smoke artifact"
```

## Follow-Up PRs

### Follow-Up A: Billing Instance Envelope Handler

Only start after Commit 4 facts extraction is reviewed.

Required before enabling `USE_INTENT_PLANNER_FOR=billing`:

- planner prompt examples for `billing_instance` vs `billing_account_unsupported`;
- handler tests for quota denial after first API call;
- handler tests for API failure after first/second call;
- tests proving billing handler does not fall back to ReAct after partial failure;
- real-account GT smoke for stopped prepaid/day, running Dynamic/Postpay, and all-instance billing grouping.

### Follow-Up B: Mixed API + KB

Only start after resource/monitor/billing envelopes are stable. Mixed handlers must combine API envelope facts with curated KB explanations, with API facts taking precedence.

## Final Verification and PR

Run:

```powershell
go test ./... -count=1
python scripts/test_planner_vs_guard_diff.py
powershell -ExecutionPolicy Bypass -File scripts/secret_scan.ps1
git diff --check origin/main..HEAD
```

Then request independent subagent review of the full branch. Address findings before opening PR.

PR title:

```text
feat: add factual envelope grounded renderer
```

PR body must state:

- default runtime remains ReAct;
- grounded renderer is opt-in;
- deterministic renderer remains fallback;
- billing cutover is not enabled in this PR;
- RAG is unchanged;
- account-level billing hard-block remains permanent;
- real-account smoke result and any known gaps.
