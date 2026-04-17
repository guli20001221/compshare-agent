# Real Account Shadow QA Round3 Freeze Summary

Date: 2026-04-17
Scope: final real-account dirty-routing pass before Block A freeze
Agent model: `doubao-seed-2-0-lite-260215`

## Goal
Re-test the remaining real-user routing gaps after the billing-stale branch was closed:

1. colloquial GPU-reference stop requests
2. explicit same-instance stop routing
3. contextual reference (`刚才那个`)
4. vague crash descriptions that should ideally disambiguate first

## Guardrails
- Only the round2 disposable instance `qa-shadow-20260417-01 / uhost-1paxrg4g1vfw` was mutated.
- The instance was returned to `Stopped` at the end of the run.
- Pre-existing failure instances were used read-only.

## Result
**2/4 PASS**

Artifact:
- [dirty_routing_report.md](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round3-freeze\dirty_routing_report.md)

## Case-by-case outcome

### PASS — `dirtyfinal_01_gpu_colloquial_needs_clarification`
Input: `那个 3080Ti 给我关了`

Observed behavior:
- No tool call was made.
- The assistant asked the user to clarify which 3080Ti instance they meant.

Conclusion:
- The worst round2 failure mode (`GetGPUSpecs` misroute) is no longer reproduced here.

### PASS — `dirtyfinal_02_explicit_name_stop_routes_workflow`
Input: `帮我关掉 qa-shadow-20260417-01`

Observed behavior:
- First tool call: `StopInstanceWorkflow`
- Stop workflow executed and the instance transitioned to `Stopping`.

Conclusion:
- The explicit-instance-name stop path is now stable in a fresh one-turn interaction.

### FAIL (route purity only) — `dirtyfinal_03_contextual_just_now_start`
Conversation:
1. `帮我关掉 qa-shadow-20260417-01`
2. `帮我开一下刚才那个`

Observed behavior:
- Both turns inserted a leading `DescribeCompShareInstance` before the workflow.
- After the pre-query, the assistant still executed:
  - `StopInstanceWorkflow`
  - `StartInstanceWorkflow`
- The second turn **did resolve** `刚才那个` to the just-mentioned instance.

Conclusion:
- User-visible behavior is mostly correct.
- Remaining gap is not failure to resolve context, but an extra pre-query before workflow routing.

### FAIL — `dirtyfinal_04_vague_crash_then_specific_failure`
Conversation:
1. `昨晚那台跑崩了`
2. `就是 wyptest 那台`

Observed behavior:
- Turn 1 immediately ran `DiagnoseInitFailure` and scanned all instances.
- It returned a list of four failed instances instead of asking a focused clarification question first.
- Turn 2 correctly narrowed onto `wyptest` and reran `DiagnoseInitFailure`.

Conclusion:
- Vague incident descriptions still trigger broad scan-all diagnosis rather than targeted disambiguation.

## What changed versus round2
- `那个 3080Ti 给我关了` improved from a wrong `GetGPUSpecs` route to a safe clarification.
- `帮我关掉 qa-shadow-20260417-01` now cleanly reaches `StopInstanceWorkflow` in the single-turn case.
- `帮我开一下刚才那个` is no longer a total miss; it resolves to the right instance, but not with workflow-first purity.
- Vague crash wording still needs product/routing cleanup.

## Remaining dirty-routing issues after round3
1. Vague failure descriptions still over-trigger broad diagnosis instead of asking for a target first.
2. Contextual-reference turns can prepend a freshness pre-query before the workflow.
3. The shadow runner currently scores route purity strictly (`first tool`), so contextual-reference success with a leading pre-query still reports FAIL.

## Final allowlist state
- `qa-shadow-20260417-01 / uhost-1paxrg4g1vfw / Stopped`

