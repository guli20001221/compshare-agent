# Real Account Shadow QA Round4 Freeze Summary

Date: 2026-04-17
Scope: Doubao-Seed-Mini validation of the remaining Block A issues
Agent model: `doubao-seed-2-0-mini-260215`

## Goal
Run the remaining freeze checks on Mini:
1. dirty routing with real-account state
2. platform-result fidelity on a safe disposable instance
3. compare Mini against the Lite live-acceptance baseline

## Guardrails
- Only disposable shadow instances were mutated:
  - `qa-shadow-20260417-01 / uhost-1paxrg4g1vfw`
  - `qa-shadow-20260417-4090 / uhost-1payastkiw8o`
- Both instances were returned to `Stopped` at the end of the run.
- No pre-existing non-shadow instances were written.

## Result
### 1. Code-side regression
- `go test ./... -count=1` PASS

### 2. Mini live acceptance
- Golden: **14/25 PASS**
- Eval: **intent 69.7% / tool 59.2% / content 77.4%**

Conclusion: Mini is substantially weaker than Lite for final acceptance and should not be used as the Block A sign-off model.

### 3. Dirty routing real-account QA
Strict runner score: **0/4 PASS**

Interpretation by case:
- `dirtymini_01_gpu_colloquial_needs_clarification`: degraded. Mini performed a leading `DescribeCompShareInstance` and then presented a single-instance shutdown confirmation instead of the safer multi-instance clarification seen on Lite.
- `dirtymini_02_explicit_name_stop_routes_workflow`: real bug. `帮我关掉 qa-shadow-20260417-01` was resolved to the wrong instance (`uhost-1pafu1vekpoe`, the older 20260416 shadow host) and incorrectly answered that it was already stopped.
- `dirtymini_03_contextual_just_now_start_accepts_fresh_query`: mixed result. Turn 1 inherited the same wrong-instance resolution as `dirtymini_02`. Turn 2 then treated `刚才那个` as that wrong 4090 host and attempted to start it. The platform rejected the start with `RetCode 8357` (no 4090 stock), and the assistant reported that failure honestly.
- `dirtymini_04_vague_crash_then_specific_failure`: partial improvement plus new resolution failure. Turn 1 correctly clarified instead of scan-all diagnosing. Turn 2 (`就是 wyptest 那台`) failed to resolve the named target and fell back to listing instances.

Conclusion: the vague-failure fix itself worked, but Mini introduced larger regressions in entity resolution and workflow routing. The remaining routing blocker is no longer only vague crash wording; Mini is broadly unreliable on write-action targeting.

### 4. Platform-result fidelity
Dedicated success probe: PASS
- Case: start `qa-shadow-20260417-4090`
- Ground truth from `hook_before`: `Stopped`
- Tool result: `StartCompShareInstance` success
- Ground truth from `hook_after`: `Starting`
- Assistant reply: successful start, consistent with actual result

Incidental failure probe: PASS
- In `dirtymini_03` turn 2, the wrongly targeted 4090 start failed with `RetCode 8357`
- Assistant reply explicitly said the start failed because `RTX4090` stock was temporarily unavailable
- This means reply fidelity was good even though routing/targeting was wrong

## Final allowlist state
- `qa-shadow-20260417-01 / uhost-1paxrg4g1vfw / Stopped`
- `qa-shadow-20260417-4090 / uhost-1payastkiw8o / Stopped`

## Artifacts
- [dirty_routing_report.md](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round4-mini-freeze\dirty_routing_report.md)
- [platform_fidelity_report.md](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round4-mini-freeze\platform_fidelity_report.md)
- [live_acceptance_summary.md](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round4-mini-freeze\live_acceptance_summary.md)