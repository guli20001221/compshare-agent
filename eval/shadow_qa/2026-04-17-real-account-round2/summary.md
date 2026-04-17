# Real Account Shadow QA Round2 Summary

Date: 2026-04-17  
Scope: deeper real-user coverage beyond Block A golden smoke tests  
Agent model: `doubao-seed-2-0-lite-260215`

## Goal
Probe three higher-fidelity categories that are underrepresented in current Block A acceptance:

1. colloquial / dirty user input
2. same-session external state changes
3. platform-failure fidelity

## Guardrails
- Only round2-created instances were mutated.
- Pre-existing instances were used read-only for diagnosis-only scenarios.
- All round2-created instances were left in `Stopped` state after testing.

## Executed coverage
### Dirty / colloquial input
Artifacts:
- [dirty_inputs_report.md](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round2\dirty_inputs_report.md)
- [dirty_contextual_report.md](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round2\dirty_contextual_report.md)

Key findings:
- `那个 3080Ti 给我关了` was misrouted to `GetGPUSpecs` instead of any stop workflow.
- `帮我关掉 qa-shadow-20260417-01` sometimes collapsed into a plain instance-detail lookup rather than `StopInstanceWorkflow`.
- `昨晚那台跑崩了` is too vague for the current agent: first turn often falls back to a generic help prompt instead of a focused instance-disambiguation prompt.
- Once a concrete failed instance is supplied (`wyptest` / `uhost-1pan4ajz9whk`), `DiagnoseInitFailure` does run and returns a plausible delete/recreate recommendation.
- The correction turn (`不是这个，是 uhost-1pan4ajz9whk 那台`) did successfully reroute diagnosis to the corrected instance in a read-only failure case, even though the first-turn vagueness handling was weak.
- `帮我开一下刚才那个` still did not reliably resolve to the just-mentioned instance or workflow path.

### External state changes in the same session
Artifact:
- [external_state_report.md](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round2\external_state_report.md)

Summary: `2/3 PASS`

Key findings:
- `ext_01_stop_after_external_start_same_session` passed and showed stop intents can survive an out-of-band restart in the same conversation.
- `ext_02_scheduler_after_external_stop_same_session` passed and proved the stale-state fix is working for scheduler requests: after an out-of-band stop, the second request re-queried and noticed the instance was no longer running.
- `ext_03_billing_after_external_stop_same_session` failed and exposed a real stale-state bug: after an out-of-band stop, the follow-up billing question did not rerun `DiagnoseBilling`, and the assistant continued to answer from the old `Running` conclusion.

### Platform failure fidelity
Artifact:
- [platform_failures_report.md](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round2\platform_failures_report.md)

Key findings:
- The planned `4090 no stock` and `3090 no stock` probes both failed to reproduce because market inventory changed during the test window. Both create requests actually succeeded.
- This is itself a useful result: stock-sensitive platform failures are highly time-dependent, so fidelity tests need either a synthetic failure harness or a broader library of candidate failure probes.
- No safe, ethical way was used to induce arrears / permission-denied / quota-exceeded on the real account.

## What is now covered better than before
- Same-session external state changes for scheduler and stop-intent refresh.
- Colloquial GPU-reference intent and contextual reference as explicit failure probes.
- Outcome-fidelity pressure on create probes, even though inventory failure did not occur during this window.

## Main gaps exposed by round2
1. Dirty input routing is still weak.
   - GPU model colloquial references can fall into `GetGPUSpecs`.
   - Vague incident descriptions can fall back to a generic help prompt instead of a focused instance disambiguation question.

2. Context reference is still weak.
   - `帮我开一下刚才那个` did not reliably resolve to the just-mentioned instance workflow path.

3. Billing follow-up still has a same-session stale-state gap.
   - Scheduler re-queries after an out-of-band stop.
   - Billing follow-up can still reuse the previous `Running` diagnosis without re-querying.

4. Platform-failure fidelity is designed but not broadly executable yet.
   - The safe real-account environment does not guarantee a reproducible inventory / arrears / quota failure on demand.

## Designed but not safely executed
- External-state-driven init-failure transition
- Account arrears / insufficient balance
- Permission denied / quota exhausted
- Intentional malformed request against the live public account

## Final allowlist state
- `qa-shadow-20260417-01` / `uhost-1paxrg4g1vfw` / `Stopped`
- `qa-shadow-20260417-4090` / `uhost-1payastkiw8o` / `Stopped`

## Artifact index
- [README.md](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round2\README.md)
- [test_matrix.md](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round2\test_matrix.md)
- [baseline_instances.md](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round2\baseline_instances.md)
- [qa_allowlist.md](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round2\qa_allowlist.md)
- [create_round2_report.md](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round2\create_round2_report.md)
- [dirty_inputs_report.md](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round2\dirty_inputs_report.md)
- [dirty_contextual_report.md](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round2\dirty_contextual_report.md)
- [external_state_report.md](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round2\external_state_report.md)
- [platform_failures_report.md](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round2\platform_failures_report.md)
