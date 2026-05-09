# Phase 1 Demo Cutover Smoke

Date: 2026-05-09

Model: `deepseek-v4-flash`

Purpose: verify the Phase 1 demo cutover path after planner prompt tuning and
form encoding for targeted instance IDs.

## Runtime

- Config source: `deploy/conf/agent.yaml.example`, copied to a temp path with
  model set to `deepseek-v4-flash`.
- Secrets: loaded from `eval/shadow_qa/.env.local` via `scripts/load_env.ps1`.
- Trace: enabled with `COMPSHARE_TRACE_ENABLED=1`.
- Cutover: enabled with `USE_INTENT_PLANNER=shadow` and
  `USE_INTENT_PLANNER_FOR=resource,monitor`.
- Raw trace, stdout, stderr, temp config, and binary were kept in a local temp
  directory and are not committed.

## Sanitized Inputs

1. `show resource info for <target>`
2. `show current CPU and GPU monitor for <target>`
3. `balance`
4. `what specs does RTX 4090 have`

## Trace Summary

| Turn | Expected path | Observed trace result |
| --- | --- | --- |
| 1 | Phase 1 resource handler | `planner.intent=resource_info`, `schema_valid=true`, `confidence=0.82`, `cutover_status=dispatched`, `planner_handler:DescribeCompShareInstance:success` |
| 2 | Phase 1 monitor handler | `planner.intent=monitor_query`, `schema_valid=true`, `confidence=0.82`, `cutover_status=dispatched`, `planner_handler:GetCompShareInstanceMonitor:success`, `freshness.monitor_call_in_current_turn=true` |
| 3 | Account billing hard-block | `engine_hard_block.hit=true`, `category=account_billing_unsupported`, no tool call |
| 4 | Legacy non-demo fallback | `planner.intent=unknown`, `cutover_status=fallback_invalid`, `knowledge_local:GetGPUSpecs:success` |

## Safety Checks

- Trace line count: 4 user turns.
- Raw target string in trace: false.
- Raw UHostId pattern in trace: false.
- Raw IPv4 pattern in trace: false.
- Sensitive marker scan in trace: false.
- Stderr bytes: 0.

## Notes

- The resource turn previously reached `failure_after_tool` because form
  encoding wrote `UHostIds` as a Go slice string. This run verified the targeted
  resource call succeeds after encoding `[]string` as `UHostIds.0`,
  `UHostIds.1`, and so on.
- No mixed-intent fixture was included in this smoke. Mixed-intent
  classification remains a Phase 1 follow-up; current demo cutover keeps
  non-resource/monitor intents on the old path unless a reviewed follow-up
  enables them.
- Both dispatched planner turns reported confidence `0.82`, matching the prompt
  examples exactly. Treat the `0.60` threshold as a demo eligibility gate, not
  as calibrated confidence evidence.
- Turn 4 reached `fallback_invalid` because the planner result fell back for an
  out-of-scope knowledge question; the old ReAct path then handled the local
  knowledge tool call.
- `renderer.input_tool_args_hashes[]` is populated for the monitor handler path.
  `renderer.input_tool_call_ids[]` remains reserved for the later renderer
  instrumentation path.
