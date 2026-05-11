# Resource Selection Smoke (2026-05-12)

## Scope

Real-account CLI smoke for the Phase 1 resource-selection clarification slice.

Runtime:
- model: `deepseek-v4-flash`
- planner mode: `shadow`
- cutover intents: `resource,monitor`
- grounded renderer: `llm`
- trace: enabled

No raw transcript, raw trace, API keys, IP addresses, UHostIds, user prompts, or billing amounts are committed.

## Scenarios

### Monitor clarification and continuation

Flow:
1. User asked a broad performance question with no instance target.
2. Planner classified it as `monitor_query`.
3. Engine returned `cutover_status=selection_required` and listed candidate instances.
4. User selected candidate number 2.
5. Engine resumed the stored monitor plan and called `GetCompShareInstanceMonitor` in the current turn.

Trace summary:

| Turn | Planner intent | Cutover status | Tool calls | Fresh monitor |
| --- | --- | --- | --- | --- |
| 1 | `monitor_query` | `selection_required` | none | false |
| 2 | `monitor_query` | `dispatched` | `GetCompShareInstanceMonitor` | true |

Result: PASS.

Notes:
- An earlier automated smoke using a PowerShell pipeline exposed a CLI issue: numeric input such as `2` was interpreted as a startup suggestion number instead of a resource-selection reply.
- The CLI now only applies startup suggestion numbers on the first user turn.

### Resource list regression

Flow:
1. User asked for all current instances.
2. User asked for running 4090-family instances.

Trace summary:

| Turn | Planner intent | Cutover status | Tool calls |
| --- | --- | --- | --- |
| 1 | `resource_info` | `dispatched` | `DescribeCompShareInstance` |
| 2 | `resource_info` | `dispatched` | `DescribeCompShareInstance` |

Observed aggregate facts:
- Total instances: 16.
- Running instances: 14.
- Stopped instances: 1.
- Initializing instances: 1.
- Running 4090-family matches: 7.

Result: PASS.

## Secret Check

The smoke trace was scanned for public/private key labels, bearer/password patterns, raw IP-like values, and billing amount/balance labels. No matches were found.
