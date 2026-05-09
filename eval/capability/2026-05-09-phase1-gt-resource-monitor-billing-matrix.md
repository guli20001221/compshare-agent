# Phase 1 GT Matrix Smoke (resource / monitor / billing)

Date: 2026-05-09

## Scope

This smoke re-runs the GT-verifiable checks with escaped Unicode inputs to avoid
PowerShell/Python codepage contamination. It covers:

- single-turn resource/basic-info question
- single-turn current monitor question
- adjacent monitor follow-up
- single-instance billing/status question
- all-instance billing/status question
- account-level billing hard-block

Raw stdout, raw trace JSONL, raw API responses, IP addresses, UHost IDs, and
secrets are not committed. Raw files stayed in a local temp directory.

Account-level inventory counts are included because they are required to verify
the all-instance billing case. Account-level financial totals, balances, and
transaction flows are not committed.

## Ground Truth

The GT baseline comes from the logged-in CompShare console plus a direct
CompShare API `DescribeCompShareInstance` / `GetCompShareInstanceMonitor` check.

Selected GT rows:

| Target | State | GPU | CPU | Memory | ChargeType | Billing note |
| --- | --- | --- | ---: | ---: | --- | --- |
| `qa-shadow-20260417-4090` | Running | 4090 x 1 | 16 | 64 GB | Dynamic | hourly/pay-as-you-go |
| `cqc-前端改配更改-预付费` | Stopped | P40 x 1 | 8 | 64 GB | Day | daily/prepaid, stopped still bills until expiry |

Per-resource currency amounts used below are published resource prices for GT
verification, not account totals, balances, or transaction-flow data.

Current monitor GT for `qa-shadow-20260417-4090`:

| Metric | Latest value |
| --- | ---: |
| CPU | 0% |
| Memory | 1% |
| GPU | 0% |
| VRAM | 1% |

Account inventory GT summary:

- Total instances: 16
- State counts: Running 13, Stopped 1, Initializing 1, Install Fail 1
- ChargeType counts: Postpay 8, Day 2, Dynamic 1, empty/no-GPU-small-instance 5
- Only stopped instance in this snapshot: `cqc-前端改配更改-预付费`, ChargeType `Day`

## Results

| Case | Input shape | Trace result | User-visible result | Verdict |
| --- | --- | --- | --- | --- |
| `single_resource_running` | One question: card/CPU/memory/state/billing for `qa-shadow` | `mixed_billing_kb`, ReAct fallback, `DescribeCompShareInstance` success | Correctly answered 4090 x1, CPU 16, 64 GB, running, Dynamic/hourly, 1.58 yuan/hour | PASS WITH NOTE |
| `single_monitor_running` | One question: current CPU/memory/GPU/VRAM for `qa-shadow` | `monitor_query`, deterministic handler, `GetCompShareInstanceMonitor` success | `CPU 0%`, `Memory 1%`, `GPU 0%`, `VRAM 1%` | PASS |
| `followup_monitor_refresh` | Turn 1 current monitor; Turn 2 asks "刚才那台 GPU 和显存现在多少？请重新查一次" | Both turns called `GetCompShareInstanceMonitor`; turn 2 used deterministic handler | Turn 2 answered `GPU 0%`, `VRAM 1%` from a current-turn monitor call | PASS |
| `single_instance_billing_stopped_prepaid` | One question about stopped prepaid `cqc` billing | `mixed_billing_kb`, ReAct fallback, `DescribeCompShareInstance` success | Correctly answered stopped, P40 x1, Day/prepaid, 8.52 yuan/day, stopped still bills | PASS WITH NOTE |
| `all_instance_billing` | One question: "所有实例分别怎么计费？哪些停机还会计费？" | `billing_instance`, ReAct fallback, `DescribeCompShareInstance` success twice | Correctly grouped by Postpay/Dynamic/Day, identified Day/prepaid stopped instances as still billing | PASS WITH NOTE |
| `account_billing_hardblock` | One question: account monthly bill/balance/transaction flow | No tool calls | Returned the expected account-level hard-block canned reply guiding user to Finance Center | PASS |

## Notes

- The first resource/basic-info question includes billing wording, so the
  planner classified it as `mixed_billing_kb` and fell back to ReAct. The answer
  was correct, but this is not yet deterministic handler coverage.
- The current monitor and monitor follow-up paths did exercise the deterministic
  monitor handler and produced clean user-facing metrics, without internal API
  paths.
- The all-instance billing answer is ReAct-based. It is acceptable for the
  current demo scope, but it is not yet a deterministic billing handler.
- The account-level hard-block behavior passed from the user-visible contract:
  no tools were called and the reply guided the user to the Finance Center.
  Trace hard-block metadata was not populated in this run; this is an
  observability follow-up, not a user-visible behavior failure.
