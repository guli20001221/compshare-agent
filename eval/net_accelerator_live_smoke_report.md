# Network Accelerator Status Live CLI Smoke

Date: 2026-06-03

Purpose: verify that `network_accelerator_status` is reachable through the default read-only routing path after adding it to the default cutover set.

Environment:

| field | value |
|---|---|
| branch | `codex/diagnosis-routing-optimization` |
| base main | `54ec27a` |
| project | `org-cwy2qk` |
| config | `deploy/conf/agent.yaml` |
| mutating tools | disabled |
| trace | enabled |

Question:

```text
网络加速现在是什么状态
```

Trace summary:

| field | value |
|---|---|
| planner intent | `network_accelerator_status` |
| planned runtime form | `routing` |
| actual runtime form | `routing` |
| realized tier | `fast` |
| cutover status | `dispatched` |
| tool calls | `CheckCompShareNetOptimizer` |
| mutating tool calls | none |

User-visible result:

The assistant returned the network acceleration status and explicitly framed it as a read-only status query. It did not claim it could enable or modify network acceleration.

Relevant runtime line:

```text
planner_mode=dispatch cutover_intents=[resource,monitor,gpu_specs,stock,pricing_query,platform_image,custom_image,community_image,network_accelerator_status]
```

