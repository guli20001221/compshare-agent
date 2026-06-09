# PR5 — stock-vs-resource BoundaryPack: live A/B parity report

**Date:** 2026-06-09
**Gate:** first intentional system-prompt SHA bump of the Intent-Router / Dispatch-Contract restructure. Prove that moving the stock-vs-resource tie-breaker from the planner base scaffold into the `internal/boundarypacks` projection does **not** change classification — *before* the flip is accepted.

## Setup

| | value |
|---|---|
| Model | `deepseek-v4-flash` (from `deploy/conf/agent.yaml`) |
| Runs per question | 5 (jitter check) |
| OLD binary | `7a9a100` (origin/main, pre-PR5: directive in base scaffold) — system prompt SHA `64dc6a4c…` |
| NEW binary | PR5 `fc0c72c` (directive projected from boundary pack) — system prompt SHA `fef7410d…` |
| Pack SHA (separate pin) | `a76bbd9c…` (`stockVsResourceBoundaryPackSHA256Baseline`) |
| Harness | `eval/pr5_boundary_ab.ps1` + `eval/pr5_boundary_questions.json` |
| Directive text | **byte-identical** between OLD and NEW; only its position in the assembled prompt changed (base → after routing directives) |

The directive moved is verbatim:

> *"Inventory availability questions like whether a GPU model has stock, is available, is sold out, or has data-center inventory are not resource_info. resource_info is only for the user's own CompShare instances. Platform stock questions should emit stock_availability."*

## Results — 10 questions × 5 runs each, per binary

| qid | kind | question | expect | OLD (×5) | NEW (×5) | parity |
|---|---|---|---|---|---|---|
| anchor-res-1 | anchor | 我有哪些实例 | resource_info | resource_info×5 | resource_info×5 | ✅ |
| anchor-res-2 | anchor | 我账号下有哪些 4090 实例 | resource_info | resource_info×5 | resource_info×5 | ✅ |
| anchor-stk-1 | anchor | 4090 有吗 | stock_availability | stock_availability×5 | stock_availability×5 | ✅ |
| anchor-stk-2 | anchor | H20 还有没有 | stock_availability | stock_availability×5 | stock_availability×5 | ✅ |
| jit-stk-1 | jitter | 现在还有 4090 吗 | stock_availability | stock_availability×5 | stock_availability×5 | ✅ |
| jit-stk-2 | jitter | 4090 卖完了吗 | stock_availability | stock_availability×5 | stock_availability×5 | ✅ |
| jit-stk-3 | jitter | 数据中心还有 H20 库存吗 | stock_availability | stock_availability×5 | stock_availability×5 | ✅ |
| jit-res-1 | jitter | 我有几台机器 | resource_info | resource_info×5 | resource_info×5 | ✅ |
| jit-res-2 | jitter | 我账号下的 A100 实例 | resource_info | resource_info×5 | resource_info×5 | ✅ |
| jit-res-3 | jitter | 我现在有哪些在跑的卡 | resource_info | resource_info×5 | resource_info×5 | ✅ |

Every cell is `distinct=1` (no jitter on either binary).

## Verdict — GATE MET

- **Historical stock/resource anchors (4): 100% parity** — OLD == NEW == expected.
- **Boundary jitter cases (6): 100% parity** — OLD == NEW == expected, all `distinct=1`.
- **50 OLD + 50 NEW = 100 live planner calls**, zero classification difference, zero jitter, zero drift.
- No finance / diagnosis directive touched (out of scope this PR).

Relocating the stock-vs-resource tie-breaker into the boundary-pack projection is **behavior-neutral**; the intentional SHA bump `64dc6a4c… → fef7410d…` carries no routing change.

> Raw per-turn traces are under `eval/traces_pr5_ab/` (git-ignored). The committed evidence is this report; the two `summary_<label>.json` files were captured during the run but are not committed.
