---
last_updated: 2026-04-17T16:05:00+08:00
---
# Project Context

## Current Status
The branch `feature/vague-failure-clarification` remains the single source of truth for the vague-failure fix. Code-side regression is green. Billing stale is fully closed. Model comparison is now richer: `Doubao-Seed-Lite` remains the only credible Block A acceptance baseline among the tested candidates. `Doubao-Seed-Mini` regressed badly on real-account routing and live eval. `Gemini-3.1-Flash-Lite` on GPTGod was technically reachable but hit heavy 429 load and is not usable for full acceptance. `Doubao-Seed-Code` is stronger than Mini and far more stable than Gemini, but still clearly weaker than Lite on workflow-first routing and real-account dirty-routing behavior, so it is also not suitable as the final sign-off model.

## In Progress
- [ ] Block A freeze decision — implementation branch is complete and code-side tests are green; remaining decision is whether to freeze using Lite as the last credible sign-off baseline or spend more work on routing-quality / eval-gap issues
- [ ] Branch integration — `feature/vague-failure-clarification` is 7 commits ahead of `master`; no remote configured yet, so push/PR is blocked until a remote is added

## Recently Completed
- [x] `Doubao-Seed-Code` candidate evaluation — live acceptance + real-account dirty routing + platform fidelity under `eval/shadow_qa/2026-04-17-real-account-round6-doubao-code/`; better than Mini, still below Lite
- [x] `Gemini-3.1-Flash-Lite` candidate evaluation — GPTGod route is reachable but heavily rate-limited (`429`), making it unsuitable for full acceptance
- [x] `Doubao-Seed-Mini` candidate evaluation — code regression green, but live acceptance and real-account dirty routing regressed severely; not suitable for sign-off
- [x] Vague-failure clarification fix on `feature/vague-failure-clarification` — prompt `vague_failure` intent + narrow `DiagnoseInitFailure` engine guard / files: `internal/engine/engine.go`, `internal/prompt/builder.go`, `internal/engine/engine_test.go` / verification: `go test ./...` PASS
- [x] Billing stale hard guard — narrow `tool_choice=DiagnoseBilling` on adjacent billing follow-up turns; real-account `ext_03` rerun PASS

## Architecture Decisions
| Decision | Choice | Reason | Date |
| --- | --- | --- | --- |
| ProjectId resolution | Config-first + runtime `GetProjectList` discovery | Inject `ProjectId` once in the executor instead of per workflow | 2026-04-17 |
| Stale-state note placement | Insert immediately before the latest user message | Keeps freshness warning close to the active ask | 2026-04-17 |
| Billing stale guard scope | Only `DiagnoseBilling`, only first ReAct round, narrow billing keyword list | Avoid hijacking same-instance restart / release / SSH turns | 2026-04-17 |
| LLM `ToolChoice` support | Pass through `any` to go-openai | Smallest hook for named tool forcing without adding hand-written NLU | 2026-04-17 |
| Vague-failure fix scope | Prompt `vague_failure` intent + `DiagnoseInitFailure` two-gate guard only | Fix the proven over-trigger without broad guard expansion | 2026-04-17 |
| Acceptance model split | Golden pass and eval-threshold pass are separate gates | `TestGoldenScripts` can pass while `TestEval` still misses threshold targets | 2026-04-17 |
| Mini test role | `Doubao-Seed-Mini` is smoke-test only, not sign-off | Round4 showed major workflow-routing and entity-resolution regressions | 2026-04-17 |
| Gemini-on-GPTGod role | Do not use for acceptance | Heavy `429` load makes results too unstable to interpret | 2026-04-17 |
| Doubao-Seed-Code role | Exploratory candidate only, not sign-off | Better than Mini, but still clearly below Lite on workflow-first routing and dirty routing | 2026-04-17 |

## Known Issues
- [high] No tested candidate has surpassed or matched the Lite baseline for Block A sign-off; Lite remains the strongest tested option despite cost concerns
- [high] `toolToIntent()` in `eval/evaluate_test.go` still classifies scheduler workflows as `simple_query`, inflating `ct_13`-`ct_16` intent failures in `TestEval`
- [medium] Outcome-fidelity assertions remain weaker than route assertions in shadow QA, although real-account probes now show both success-fidelity and failure-fidelity examples
- [medium] `Doubao-Seed-Code` still overuses leading `DescribeCompShareInstance` before workflows and mishandles colloquial GPU-reference shutdown requests
- [medium] `Doubao-Seed-Code` over-applies the vague-failure clarification on turn 2 (`就是 wyptest 那台`) instead of proceeding into `DiagnoseInitFailure`
- [medium] No remote is configured, so `feature/vague-failure-clarification` cannot be pushed or reviewed via PR yet