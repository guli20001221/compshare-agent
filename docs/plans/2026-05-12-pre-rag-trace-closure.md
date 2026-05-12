# Pre-RAG Trace Closure Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Finish the pre-RAG trace cleanup so trace output is smaller, safer, and operationally useful before Stage 2B RAG runtime work starts.

**Architecture:** Keep trace writing best-effort and non-blocking for user requests. Add only observable metadata: omit empty trace fields, record real LLM token totals when provider returns usage, run retention cleanup on CLI startup, and ensure trace hashes see the same redacted/sanitized payloads that the LLM sees. Do not add hallucination counters until a real detector exists.

**Tech Stack:** Go, JSONL trace writer, go-openai streaming usage, existing SafeToolExecutor policies, existing CLI recorder tests.

---

## Current State

- `internal/observability/trace.go` already uses `SchemaVersion = "trace.v0.2"` and has cap fields (`capped`, `requested_targets`, `window_seconds`, etc.).
- v0.2 fields are currently always emitted, including many zero-value nested blocks.
- `observability.Cleanup()` exists and is tested, but CLI startup does not call it.
- `llm.ChatResponse` does not expose usage. The underlying `go-openai` stream response supports `stream_options.include_usage`.
- `SafeToolExecutor` applies `policy.RedactInResult` to `LLMResult`, but `TraceResult` only uses `security.RedactForTrace`.
- Some `OnBeforeCall` step events pass filtered args directly instead of `safeExecutor.RedactArgs`, so trace hashing still relies on generic trace redaction as the last line of defense.

## Non-Goals

- Do not implement hallucination counters.
- Do not change RAG retrieval behavior.
- Do not change tool routing, planner behavior, resource selection, renderer output, or safety policy decisions.
- Do not make trace failures block CLI startup or user turns.
- Do not remove v0.1 fixture compatibility.

## Task 1: Make Trace v0.2 Sparse With Marshal-Time Projection

**Files:**
- Modify: `internal/observability/trace.go`
- Modify: `internal/observability/trace_test.go`
- Modify if needed: `internal/observability/testdata/trace_v0_2_cap_fields.json`

**Steps:**
1. Add a marshal-time projection for `TraceRecord` rather than relying on `omitempty` on ordinary struct fields. Ordinary Go struct fields are never omitted just because all children are zero.
2. Keep core identity fields always present: `schema_version`, `trace_id`, `turn_id`, `turn_index`, `timestamp`, `user_msg_hash`.
3. Omit optional top-level blocks only when they are completely unobserved: planner, engine_hard_block, entity_registry, renderer, freshness, rate_limit, retrieval, and outcome.
4. When an optional top-level block is emitted, preserve meaningful false/zero values inside that block. Examples: `planner.schema_valid=false`, `renderer.fallback_used=false`, `rate_limit.allowed=false`, and `tool_calls[].executed_targets=0` are real states and must not disappear from a populated block.
5. Keep `runtime` visible when planner mode or cutover intents are present. Keep `tool_calls` visible when at least one call exists.
6. Keep arrays that are required as arrays when their parent block is emitted, but allow parent blocks to disappear when fully empty.
7. Preserve `withDefaults()` for backwards-compatible in-memory defaults before marshal and for in-memory test assertions.
8. Update tests so a minimal trace no longer requires zero-value `rate_limit` or other empty optional blocks to be present.
9. Add a compatibility test that unmarshalling a sparse record missing planner/rate_limit/retrieval/outcome succeeds and leaves zero values.
10. Add a test that confirms non-empty cap fields still appear in v0.2 JSON, including `executed_targets: 0` for blocked calls.

**Verification:**
- `go test ./internal/observability -count=1`

## Task 2: Record Real LLM Total Tokens Only When Available

**Files:**
- Modify: `internal/llm/client.go`
- Modify: `internal/llm/client_test.go`
- Modify: `internal/engine/engine.go`
- Modify: `internal/intent/planner.go`
- Modify: `internal/intent/shadow.go`
- Modify: `internal/intent/trace_projection.go`
- Modify: `internal/renderer/renderer.go`
- Modify: `cmd/trace.go`
- Modify: `cmd/agent.go`
- Modify: `cmd/trace_test.go`

**Steps:**
1. Extend `llm.ChatResponse` with a usage field carrying actual provider usage (`PromptTokens`, `CompletionTokens`, `TotalTokens` or equivalent).
2. Add an internal capability fallback in `llm.Client`: first try streaming with `StreamOptions.IncludeUsage = true`; if the provider rejects the request because `stream_options` / usage is unsupported, retry once without usage and return a response with zero usage. Non-usage errors must keep existing retry behavior.
3. In the stream receive loop, read `chunk.Usage` before checking `len(chunk.Choices)==0`; the final usage chunk has empty choices.
4. Change planner LLM plumbing so `CompleteIntentPlan` can return raw text plus usage. The engine and shadow runner should keep existing behavior but also surface usage into trace when present.
5. Extend `renderer.RenderResult` with usage so grounded renderer calls contribute actual usage.
6. Add an engine observer or trace callback that aggregates actual usage totals from main ReAct, planner, shadow planner, and grounded renderer calls where usage is available.
7. Record the aggregate into `OutcomeTrace.TotalTokens`.
8. If usage is unavailable or zero, omit `total_tokens`; do not estimate.

**Verification:**
- Unit test a mock/stream response path where usage is present.
- `go test ./internal/llm ./internal/engine ./cmd -count=1`

## Task 3: Run Trace Retention Cleanup On CLI Startup

**Files:**
- Modify: `cmd/trace.go`
- Modify: `cmd/agent.go`
- Modify: `cmd/trace_test.go`

**Steps:**
1. Add a helper that calls `observability.Cleanup(writer.Dir(), observability.DefaultTraceRetentionDays, time.Now())`.
2. Call it immediately after trace writer creation succeeds.
3. On cleanup error, write a warning to stderr and continue with trace enabled.
4. Add tests for successful cleanup and cleanup warning behavior.

**Verification:**
- `go test ./cmd -count=1`

## Task 4: Align Trace Args/Result Redaction With SafeToolExecutor Policy

**Files:**
- Modify: `internal/tools/safe_executor.go`
- Modify: `internal/tools/safe_executor_test.go`
- Modify: `internal/engine/engine.go`
- Modify: `cmd/trace_test.go`

**Steps:**
1. Apply `policy.RedactInResult` to `TraceResult` before hashing, matching `LLMResult` field-level redaction.
2. Keep generic `security.RedactForTrace` as the final safety layer.
3. In direct ReAct `OnBeforeCall`, planner-handler `OnBeforeCall`, and any other StepToolCall emission that originates from SafeToolExecutor hooks, pass `safeExecutor.RedactArgs(action, args)` instead of raw filtered args.
4. In blocked/friendly-error paths that attach args to trace events, pass sanitized args where a SafeToolExecutor policy exists. This includes direct ReAct cap/rate-limit errors and planner-handler friendly errors.
5. Keep knowledge-local paths using filtered args because they bypass SafeToolExecutor execution by design.
6. Add tests proving:
   - a policy-only redacted result field changes `TraceResult` and does not leak into trace hash input,
   - `OnBeforeCall` redacts password-like args before the recorder receives them,
   - existing hash-level redaction still catches raw values if a future path forgets to sanitize.

**Verification:**
- `go test ./internal/tools ./internal/engine ./cmd -count=1`

## Final Verification

Run:

```powershell
go test ./... -count=1
python scripts/test_planner_vs_guard_diff.py
git diff --check
scripts/secret_scan.ps1
```

Expected:
- All commands pass.
- Trace lines are still one JSON object per turn.
- No raw secret, billing amount, bearer token, IP, password, Jupyter token, or private key appears in trace output.
- Empty trace sections are absent or minimal, while populated planner/retrieval/renderer/rate-limit/cap fields remain visible.
