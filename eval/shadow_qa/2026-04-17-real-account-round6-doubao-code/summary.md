# Real Account Shadow QA Round6 Summary

Date: 2026-04-17
Scope: `Doubao-Seed-Code` candidate evaluation after Mini and Gemini
Agent model: `doubao-seed-2-0-code-preview-260215`

## Goal
Determine whether `Doubao-Seed-Code` can replace Lite as a viable acceptance model by checking:
1. live golden + eval behavior
2. dirty routing on real-account scenarios
3. platform-result fidelity on safe disposable instances

## Guardrails
- Only disposable shadow instances were mutated:
  - `qa-shadow-20260417-01 / uhost-1paxrg4g1vfw`
  - `qa-shadow-20260417-4090 / uhost-1payastkiw8o`
- Both instances were returned to `Stopped` at the end of the run.
- No pre-existing non-shadow instances were written.

## Result
### 1. Live acceptance
- Golden: **15/25 PASS**
- Eval: **intent 84.2% / tool 65.3% / content 84.4%**

Interpretation:
- Better than Mini and dramatically better than Gemini-on-GPTGod
- Still clearly below the Lite baseline (`25/25` golden and `92.1 / 93.9 / 81.2` eval)
- Not acceptable as the Block A sign-off model

### 2. Dirty routing real-account QA
Strict runner score: **1/4 PASS**

Interpretation by case:
- `dirtymini_01_gpu_colloquial_needs_clarification`: wrong. The model skipped clarification and went straight into `StopInstanceWorkflow` for one 3080Ti instance.
- `dirtymini_02_explicit_name_stop_routes_workflow`: user-visible behavior was correct, but route purity still had a leading `DescribeCompShareInstance` before the workflow.
- `dirtymini_03_contextual_just_now_start_accepts_fresh_query`: PASS. The contextual `刚才那个` path resolved correctly and both stop/start succeeded.
- `dirtymini_04_vague_crash_then_specific_failure`: mixed. Turn 1 correctly clarified vague failure. Turn 2 over-clarified again instead of proceeding into `DiagnoseInitFailure`, so the fix overshot for this model.

Conclusion: `Doubao-Seed-Code` is usable for exploratory QA but still not clean enough for final freeze. It behaves better than Mini, but not as well as Lite on the most important write-action and follow-up-routing paths.

### 3. Platform-result fidelity
Dedicated probe: PASS
- Case: start `qa-shadow-20260417-4090`
- Ground truth from `hook_before`: `Stopped`
- Ground truth from `hook_after`: `Running`
- Assistant reply reported successful start consistently with the actual result

## Final allowlist state
- `qa-shadow-20260417-01 / uhost-1paxrg4g1vfw / Stopped`
- `qa-shadow-20260417-4090 / uhost-1payastkiw8o / Stopped`

## Artifacts
- [dirty_routing_report.md](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round6-doubao-code\dirty_routing_report.md)
- [platform_fidelity_report.md](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round6-doubao-code\platform_fidelity_report.md)
- [live_acceptance_summary.md](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round6-doubao-code\live_acceptance_summary.md)