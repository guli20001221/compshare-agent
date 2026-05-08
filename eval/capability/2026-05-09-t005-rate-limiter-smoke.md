# T-005 RateLimiter Smoke

Date: 2026-05-09

Scope: local/mock smoke only. No real LLM provider calls and no real mutating CompShare APIs were invoked.

## Command Shape

All commands ran from the repository root in the T-005 worktree.

```powershell
go test ./internal/governance -run TestMemoryLimiterQPSLimit -count=1 -v
go test ./internal/engine -run "TestChat_LLMRateLimitDenialSkipsLLM|TestChat_MutatingRateLimitDenialSkipsConfirmAndExecutor" -count=1 -v
go test ./internal/intent -run TestShadowRunner_QuotaDenialSkipsPlannerAndReturnsInvalidTrace -count=1 -v
go test ./cmd -run TestCLITraceRecorderWritesRateLimitDenial -count=1 -v
go test ./... -count=1
powershell -ExecutionPolicy Bypass -File scripts\secret_scan.ps1
```

## Limits Used

The core limiter smoke used the fake-clock unit test with:

| Class | QPS | Daily |
| --- | ---: | ---: |
| LLM | 2 | 100 |
| Mutating tool | 1 | 50 |

Engine, shadow runner, and trace smoke paths used scripted in-process limiter decisions to avoid provider calls and real mutating API execution.

## Denial Results

| Path | Trigger | Expected behavior | Result |
| --- | --- | --- | --- |
| Core limiter | Third LLM request in same QPS window | `qps_exceeded` with positive retry-after | PASS |
| Main LLM | Scripted LLM quota denial | Skip `llmClient.Chat`, return fixed QPS message | PASS |
| Mutating tool | Scripted mutating quota denial | Skip L1 confirmation and executor call | PASS |
| Shadow planner | Scripted planner quota denial | Skip planner LLM, emit invalid unknown planner trace | PASS |
| Trace writer | Scripted rate-limit decision | Write additive `rate_limit` block without raw subject | PASS |

## Trace Rate-Limit Block

The trace smoke writes one JSONL line and verifies an additive `rate_limit` block equivalent to:

```text
checked=true
allowed=false
class=llm
action=shadow_planner
reason=qps_exceeded
subject_hash=sha256:<synthetic>
retry_after_ms=200
```

The test also asserts that the raw public-key-like test input is absent from the trace line.

## Verification Output

```text
TestMemoryLimiterQPSLimit PASS
TestChat_LLMRateLimitDenialSkipsLLM PASS
TestChat_MutatingRateLimitDenialSkipsConfirmAndExecutor PASS
TestShadowRunner_QuotaDenialSkipsPlannerAndReturnsInvalidTrace PASS
TestCLITraceRecorderWritesRateLimitDenial PASS
go test ./... -count=1 PASS
scripts/secret_scan.ps1 PASS
```

## Secret Hygiene

This artifact intentionally excludes raw public/private keys, LLM API keys, ProjectId values, UHostIds, IP addresses, raw user prompts, provider responses, raw trace JSONL, and raw tool payloads.

## Deferred Real-Account Validation

Real-account rate-limit verification is intentionally deferred. T-007b real-account shadow smoke should exercise the LLM quota path on the primary `deepseek-v4-flash` baseline without dedicated T-005 re-runs. That run should also watch for `rate_limit.reason="qps_exceeded"` under baseline defaults (LLM 5 QPS / 5000 daily, mutating 1 QPS / 50 daily), because ReAct multi-round bursts may approach the 5 QPS bucket.
