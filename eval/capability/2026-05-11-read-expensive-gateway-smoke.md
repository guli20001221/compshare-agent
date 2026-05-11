# Read-Expensive Gateway Real-Account Smoke

Date: 2026-05-11

Scope: T-Hardening.ReadExpensiveGateway commit 6 smoke. This artifact summarizes real-account and controlled mock verification for the read-expensive gateway, trace.v0.2 cap metadata, and the default ReAct + `deepseek-v4-flash` runtime path.

This is a gateway/trace smoke, not an answer-quality benchmark. Raw transcripts, raw trace JSONL, raw API responses, keys, IPs, UHostIds, ProjectId values, and full tool payloads are intentionally excluded.

## Runtime

Primary real-account run:

```text
model=deepseek-v4-flash
planner_mode=shadow
cutover_intents=[]
trace_enabled=true
schema_version=trace.v0.2
read_expensive_qps=default
read_expensive_daily=default
```

The CLI startup/runtime trace stayed on ReAct with shadow-only planner observation. No deterministic cutover was enabled.

## Primary Real-Account Run

The run used five turns: one warm-up turn plus four scripted verification scenarios.

| Scenario | Expected gateway behavior | Trace result |
| --- | --- | --- |
| Current running inventory | `DescribeCompShareInstance` succeeds under default read-expensive limits | PASS: one successful `DescribeCompShareInstance`, no cap |
| Current monitor for one known instance | `GetCompShareInstanceMonitor` succeeds with one target | PASS: one successful monitor call, `requested_targets=1`, `executed_targets=1`, `freshness.monitor_call_in_current_turn=true` |
| All-running monitor below cap | List inventory, then monitor all running targets; cap is 20 | PASS: monitor call succeeded with `requested_targets=13`, `executed_targets=13`, `capped=""` |
| Account-level billing / balance | Engine hard-block, no tool calls | PASS: `engine_hard_block.hit=true`, category `account_billing_unsupported`, `tool_calls=[]` |

Trace summary:

```text
records=5
schema_versions=["trace.v0.2"]
runtime=["shadow:[]"]
stderr_chars=0
stdout_error_signal=false
monitor_calls=2
describe_calls=6
hard_block_turns=1
```

Sanitized inventory probe:

```text
total_instances=19
returned_instances=19
running_instances=13
stopped_instances=2
truncated=false
```

The ticket was written when the real-account inventory was expected to be in the 16-18 instance range; at smoke time the account had 19 total instances. The all-running monitor scenario proves the current running target set is below the `MaxTargetsPerCall=20` monitor cap and is not accidentally blocked by the new defaults. The inventory scenario also proves list-style instance discovery is allowed under the default read-expensive quota.

## Read-Expensive Quota Visibility

A second real-account run used an intentionally low local config to force a read-expensive denial:

```text
read_expensive_qps=50
read_expensive_daily=1
```

The first user-visible inventory request was denied after the startup refresh consumed the single daily read-expensive unit.

Trace result:

```text
rate_limit.class=read_expensive_tool
rate_limit.action=DescribeCompShareInstance
rate_limit.allowed=false
rate_limit.reason=daily_exceeded
tool_calls[0].action=DescribeCompShareInstance
tool_calls[0].status=error
tool_calls[0].capped=rate_limit
tool_calls[0].executed_targets=0
```

This verifies that read-expensive quota decisions are observable in trace.v0.2. It also verifies the denial path does not expose raw request arguments in the artifact.

## Controlled Over-Target Cap

The real account currently has fewer than 21 running monitor targets, so the over-target case was verified through the controlled mock seam required by the ticket.

Commands:

```powershell
go test ./internal/engine -run "TestChat_ReadExpensiveTargetCapBecomesToolResult|TestChat_ReadExpensiveRateLimitDenialBecomesToolResult" -count=1 -v
go test ./cmd -run "TestCLITraceRecorderWritesBlockedCapFields" -count=1 -v
```

Result:

```text
TestChat_ReadExpensiveTargetCapBecomesToolResult PASS
TestChat_ReadExpensiveRateLimitDenialBecomesToolResult PASS
TestCLITraceRecorderWritesBlockedCapFields PASS
```

The engine test verifies a monitor request over the target cap returns the friendly cap result and does not proceed as a normal tool execution. The trace test verifies blocked cap metadata is represented as `capped="targets"` with a cap reason.

## Secret Hygiene

This artifact does not include:

- raw trace JSONL
- raw stdout / transcript text
- raw API response payloads
- public/private keys
- LLM API keys
- ProjectId values
- UHostIds
- IP addresses
- raw target argument lists
- billing amounts or account balances

Raw run files were kept only in a local temp directory during verification and are not committed.

## Known Limitations

- The primary real-account run is scripted to exercise gateway and trace paths. It should not be used as evidence that natural-language answer quality is acceptable.
- The top-level trace rate-limit block stores the first denial or the latest allow by design. For successful read-expensive calls in a turn that later runs planner shadow, the final allow record can be `llm/shadow_planner`; denial cases remain visible because first denial wins.
- The over-target path uses a mock seam because the current account does not exceed the monitor target cap.
