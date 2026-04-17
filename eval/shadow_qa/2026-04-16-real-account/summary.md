# Real Account Shadow QA Summary

Date: 2026-04-16  
Scope: Block A real-account shadow QA against the logged-in 优云控制台 account  
Agent model: `doubao-seed-2-0-mini-260215`

## Scope
- Build a dedicated shadow-QA binary and config separate from prior golden assets.
- Establish a write allowlist containing only instances created during this run.
- Exercise real-account scenarios against the CLI agent and compare them with actual control-panel state.
- Record failures, risks, and concrete integration gaps exposed only by live state.

## Baseline and guardrails
- Pre-existing instances were recorded before any write operation in [baseline_instances.md](F:\compshare-agent\eval\shadow_qa\2026-04-16-real-account\baseline_instances.md).
- Only the allowlisted instance in [qa_allowlist.md](F:\compshare-agent\eval\shadow_qa\2026-04-16-real-account\qa_allowlist.md) was mutated.
- Final console screenshot was saved to [console-final-state.png](F:\compshare-agent\eval\shadow_qa\2026-04-16-real-account\artifacts\console-final-state.png).

## Executed scenarios
### Create probes
| Case | Result | What happened |
| --- | --- | --- |
| `shadow_01_create_test_instance` | FAIL | A realistic 4090 create request produced free-text confirmation and did not call `CreateInstanceWorkflow`. |
| `shadow_01b_create_3090_stock_probe` | PASS | The agent called `CreateInstanceWorkflow`, hit inventory check, and correctly explained that 3090 stock was unavailable in `cn-wlcb-01`. |

### Round 1
| Case | Result | What happened |
| --- | --- | --- |
| `shadow_02_stop_instance` | FAIL | First-turn stop intent produced free-text confirmation only; no `StopInstanceWorkflow` call happened. |
| `shadow_03_billing_after_stop` | PASS | Billing diagnosis correctly said the instance was still `Running`, revealing that the earlier stop had not actually executed. |
| `shadow_04_start_instance` | PASS | The agent correctly identified the instance was already running and did not attempt a redundant start. |
| `shadow_05_reboot_instance` | PASS | `RebootInstanceWorkflow` executed successfully on the allowlisted test instance. |
| `shadow_06_set_scheduler` | PASS by current assertions, FAILED operationally | `SetStopSchedulerWorkflow` hit `UpdateCompShareStopScheduler`, but the real API failed with `Params [ProjectID] not available`. |
| `shadow_07_cancel_scheduler` | PASS | `CancelStopSchedulerWorkflow` executed successfully. |

### Follow-up
| Case | Result | What happened |
| --- | --- | --- |
| `shadow_08_stop_two_turn_confirm` | PASS | First turn produced conversational confirmation; second-turn `确认` triggered `StopInstanceWorkflow` and actual stop succeeded. |
| `shadow_09_billing_after_real_stop` | PASS | Billing diagnosis correctly explained that post-stop instance fee was zero. |
| `shadow_10_start_after_real_stop` | PASS by current assertions, FAILED operationally | `StartInstanceWorkflow` ran, but actual start failed with `RetCode=8357` because 4090 inventory was unavailable. |

## Final control-panel state
The allowlisted instance was left in a safe stopped state:
- Name: `qa-shadow-20260416-01`
- UHostId: `uhost-1pafu1vekpoe`
- Final observed state: `关机`
- Release time shown in console: `2026-04-23 17:16`

## Confirmed strengths
- Real reboot on an allowlisted instance works.
- Real stop works when the conversational confirmation path completes.
- Real cancel-scheduler works.
- Billing diagnosis aligns with real running/stopped state.
- Create flow can surface real inventory shortage and suggest alternative GPU types.

## Failure points
1. Create intent is still brittle in live usage.
   - A realistic 4090 create request did not trigger `CreateInstanceWorkflow`.
   - The agent fell back to plain-text confirmation instead of the real workflow path.

2. Stop intent currently depends on a conversational confirmation turn.
   - A direct first-turn stop request did not call the workflow.
   - The same request worked only after a second-turn `确认`.

3. Set scheduler is not operational on the public-account path.
   - Live API call failed with `Params [ProjectID] not available`.
   - This is a real integration issue, not a mock-only problem.

4. Workflow success and operational success are not the same.
   - The start workflow executed correctly, but the platform refused the operation because inventory was unavailable.
   - Current test assertions still mark this as pass because they verify workflow routing more strongly than platform outcome.

## Risk list
### High
- Live create and stop intents can degrade into free-text confirmation without hitting the intended workflow.
- Scheduler stop is exposed as a supported capability, but the live API path currently fails on account/project parameter requirements.

### Medium
- Start/restart style operations can pass workflow-level checks while still failing on real-time inventory; acceptance needs to distinguish tool success from platform success.
- Real CLI shadow tests are now much stronger than offline golden, but some cases still validate routing more strongly than final platform outcome.

### Low
- The shadow-QA instance remains stopped but not terminated. It is still inside the allowlist and can be cleaned up later if desired.

## Recommendations
1. Add a real-account regression specifically for the 4090 create free-text fallback.
2. Tighten CLI acceptance for start/scheduler cases so platform-level failure is visible as failure, not only workflow routing success.
3. Investigate how `ProjectID` is injected for `UpdateCompShareStopScheduler` and align it with the paths used by successful live write operations.
4. Treat first-turn free-text confirmations on write intents as a UX bug unless they deterministically transition into the same guarded workflow path.

## Artifact index
- [README.md](F:\compshare-agent\eval\shadow_qa\2026-04-16-real-account\README.md)
- [baseline_instances.md](F:\compshare-agent\eval\shadow_qa\2026-04-16-real-account\baseline_instances.md)
- [qa_allowlist.md](F:\compshare-agent\eval\shadow_qa\2026-04-16-real-account\qa_allowlist.md)
- [create_report.md](F:\compshare-agent\eval\shadow_qa\2026-04-16-real-account\create_report.md)
- [create_3090_report.md](F:\compshare-agent\eval\shadow_qa\2026-04-16-real-account\create_3090_report.md)
- [round1_report.md](F:\compshare-agent\eval\shadow_qa\2026-04-16-real-account\round1_report.md)
- [followup_report.md](F:\compshare-agent\eval\shadow_qa\2026-04-16-real-account\followup_report.md)
- [console-final-state.png](F:\compshare-agent\eval\shadow_qa\2026-04-16-real-account\artifacts\console-final-state.png)
