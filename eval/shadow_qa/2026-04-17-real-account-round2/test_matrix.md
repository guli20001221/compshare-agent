# Round2 Real-Account Shadow QA Test Matrix

Date: 2026-04-17  
Account: `compshare-test@ucloud.cn`  
Agent model: `doubao-seed-2-0-lite-260215`

## Goal
Push beyond Block A golden smoke coverage and probe three classes of real-user behavior:

1. `脏输入 / 口语化输入`
2. `外部状态变化`
3. `平台失败但 agent 要如实转述`

## Guardrails
- Only mutate instances created during this round.
- Read-only checks may reference pre-existing instances for diagnosis-only scenarios.
- Real platform failures are not counted as agent failures unless the final reply misrepresents the platform result.

## Round2 Allowlist
- `qa-shadow-20260417-01`
  - `UHostId`: `uhost-1paxrg4g1vfw`
  - `GPU`: `3080Ti x1`
  - `Image`: `Ubuntu-nvidia 22.04`
  - `ChargeType`: `Dynamic`

## Scenario Matrix
| ID | Category | Scenario | Type | Expected behavior | Execution |
| --- | --- | --- | --- | --- | --- |
| `dirty_01` | Dirty input | `那个 3080Ti 给我关了` | write | Route to `StopInstanceWorkflow`; stop the allowlisted instance | execute |
| `dirty_02` | Dirty input | `昨晚那台跑崩了` -> `就是 wyptest 那台` | read-only multi-turn | First turn asks which instance; second turn routes to `DiagnoseInitFailure` | execute |
| `dirty_03` | Dirty input | `昨晚那台跑崩了` -> `就是 wyptest 那台` -> `不是这个，是 uhost-1pan4ajz9whk 那台` | read-only multi-turn | Correction turn should rerun diagnosis for the corrected instance | execute |
| `dirty_04` | Dirty input | `帮我开一下刚才那个` after a previous explicit write turn | write multi-turn | Resolve `刚才那个` from same-session context and route to the correct workflow | execute |
| `ext_01` | External state | Scheduler request after external stop in the same session | mixed | After an out-of-band stop, the second scheduler request must fresh-query and notice `Stopped` | execute |
| `ext_02` | External state | Billing follow-up after external stop in the same session | read-only | Follow-up reply must reflect new stopped state and mention residual disk/image cost only | execute |
| `ext_03` | External state | Init-failure diagnosis after external state change | mixed | Ideally re-query and reflect latest install state | design only |
| `pf_01` | Platform failure fidelity | 4090 create request with no stock | write attempt | Route through `CreateInstanceWorkflow`; reply must clearly say failure / no stock | execute |
| `pf_02` | Platform failure fidelity | Insufficient balance / arrears | write attempt | Reply must honestly surface account failure from platform | design only |
| `pf_03` | Platform failure fidelity | Permission denied / quota exhausted | write attempt | Reply must honestly surface platform-side denial | design only |
| `pf_04` | Platform failure fidelity | Parameter missing / malformed request | write attempt | Reply must surface platform error, not fake success | design only |

## Notes
- `ext_03` is not safely inducible on the current public-account path without deliberately destabilizing an instance into an install-failure transition.
- `pf_02` / `pf_03` / `pf_04` are intentionally left as design-only cases because they would require account-level impairment or deliberate system misconfiguration outside the safe QA boundary.
