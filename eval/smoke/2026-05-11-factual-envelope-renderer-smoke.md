# Phase 1 factual envelope renderer smoke (ds v4 flash)

Date: 2026-05-11

Branch: `codex/phase1-factual-envelope-renderer`

Model: `deepseek-v4-flash`

Runtime flags:

- `USE_INTENT_PLANNER=shadow`
- `USE_INTENT_PLANNER_FOR=resource,monitor`
- `USE_GROUNDED_RENDERER=llm`
- `COMPSHARE_TRACE_ENABLED=1`

This artifact summarizes two real-account CLI smoke runs. It intentionally does not commit raw transcripts, raw trace files, raw API payloads, UHost IDs, IPs, project IDs, keys, account balances, or billing amounts.

## Scope

This smoke verifies the Phase 1 factual-envelope renderer path for resource and current-monitor answers:

- planner cutover can dispatch `resource_info` and `monitor_query`;
- the handler builds a customer-safe factual envelope from API facts;
- the grounded renderer either renders from the envelope or safely falls back to the deterministic envelope summary;
- trace.v0.2 records planner / renderer / tool-call metadata without raw sensitive payloads;
- account-level billing remains hard-blocked and does not enter LLM/tool execution.

Billing-instance envelope rendering is not enabled in this branch. Commit 4 only extracted billing facts for future work.

## Targeted Phase 1 path smoke

Prompt shape:

1. single-instance resource info;
2. single-instance current CPU/GPU monitor.

The target was an existing running GPU instance. The target name and UHost ID are omitted here.

| Turn | Expected path | Observed trace result | User-visible result summary |
| --- | --- | --- | --- |
| 1 | `resource_info` handler + grounded renderer | `schema_valid=true`, `intent=resource_info`, `cutover_status=dispatched`, tool=`DescribeCompShareInstance`, renderer=`rendered`, `envelope_kind=resource_info` | Answer included state, OS, CPU, memory, GPU type/count, region/zone, charge type, auto-renew. |
| 2 | `monitor_query` handler + grounded renderer | `schema_valid=true`, `intent=monitor_query`, `slots.metrics=[cpu,gpu]`, `cutover_status=dispatched`, tool=`GetCompShareInstanceMonitor`, `freshness.monitor_call_in_current_turn=true`, renderer=`rendered`, `envelope_kind=monitor_query` | Answer returned CPU and GPU utilization from semantic monitor facts. GPU bus IDs were not used as user-visible fact labels. |

Trace summary:

- Trace records: 2
- Schema: `trace.v0.2`
- Renderer status: 2 rendered, 0 fallback
- Tool calls: `DescribeCompShareInstance` x1, `GetCompShareInstanceMonitor` x1
- Trace raw leak scan: 0 UHost ID hits, 0 bearer hits, 0 private-key label hits, 0 IPv4 hits

Important observation: this targeted English prompt is the current positive path for Phase 1 resource/monitor cutover. Natural Chinese monitor prompts below still exposed planner classification gaps and fell back to ReAct.

## Natural Chinese smoke

Prompt shape:

1. single-instance resource info;
2. list running machines;
3. single-instance CPU / memory / GPU / VRAM monitor;
4. all-running-instance monitor summary;
5. account-level monthly bill / balance question.

| Turn | Observed trace result | User-visible result summary |
| --- | --- | --- |
| 1 | `intent=resource_info`, `schema_valid=true`, `cutover_status=dispatched`, renderer=`rendered`, tool=`DescribeCompShareInstance` | Answer summarized the target instance resource facts from the envelope. |
| 2 | `intent=resource_info`, `schema_valid=true`, `cutover_status=dispatched`, renderer=`fallback`, `fallback_reason=validation_failed`, tool=`DescribeCompShareInstance` | Fallback returned the deterministic instance list. This is safe but less polished than the old ReAct table-style answer. |
| 3 | planner invalid fallback, production path tool=`GetCompShareInstanceMonitor`, `freshness.monitor_call_in_current_turn=true` | ReAct answered current CPU / memory / GPU / VRAM facts from a fresh monitor call. |
| 4 | planner invalid fallback, production path tools=`DescribeCompShareInstance`, `GetCompShareInstanceMonitor`, `freshness.monitor_call_in_current_turn=true` | ReAct answered all-running-instance monitor facts from fresh Describe + Monitor calls. |
| 5 | `engine_hard_block.hit=true`, `category=account_billing_unsupported`, no tool calls | Account-level bill / balance question returned the canned console-guidance reply. |

Trace summary:

- Trace records: 5
- Schema: `trace.v0.2`
- Renderer status: 1 rendered, 1 validation fallback, 3 not invoked
- Tool calls: `DescribeCompShareInstance` x3, `GetCompShareInstanceMonitor` x2
- Trace raw leak scan: 0 UHost ID hits, 0 bearer hits, 0 private-key label hits, 0 IPv4 hits

## Findings

1. The factual-envelope renderer path works for single-instance resource info under ds v4 flash.
2. The monitor handler path works for a targeted monitor query, and the current implementation now extracts semantic monitor facts from the real `GetCompShareInstanceMonitor` API shape before rendering or falling back.
3. Monitor renderer fallback remains required by design even though the final targeted smoke rendered successfully.
4. Natural Chinese monitor questions in this smoke were still classified invalid by the planner and fell back to the production ReAct path. This branch does not attempt planner prompt tuning.
5. All-instance resource listing is safe but visually rough when renderer validation falls back to the deterministic list.
6. Account-level billing hard-block behavior remains intact.

## Non-goals confirmed

- No raw trace/transcript/API payload committed.
- No RAG or FAQ retrieval changes.
- No billing-instance cutover.
- No account-level billing enablement.
- No attempt to make planner prompt tuning part of this branch.
