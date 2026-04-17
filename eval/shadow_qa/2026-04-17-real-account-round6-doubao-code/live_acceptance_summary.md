# Live Acceptance Summary (Doubao-Seed-Code)

Date: 2026-04-17
Model: `doubao-seed-2-0-code-preview-260215`

## Commands
- `go test -count=1 -timeout 45m ./eval -run TestGoldenScripts -v -args -model "Doubao-Seed-Code"`
- `go test -count=1 -timeout 45m ./eval -run TestEval -v -args -model "Doubao-Seed-Code"`

## Result
- Golden live acceptance: **15/25 PASS**, **10/25 FAIL**
- Eval live acceptance: **intent 84.2% (64/76)** / **tool 65.3% (32/49)** / **content 84.4% (27/32)**

## Key observations
1. `Doubao-Seed-Code` is materially more stable than `Gemini-3.1-Flash-Lite` and stronger than `Doubao-Seed-Mini` on both golden and eval.
2. It is still clearly weaker than the Lite baseline for Block A sign-off. The main failure mode remains workflow-first routing: many write-action cases prepend `DescribeCompShareInstance` or stay in a confirmation-style answer instead of entering the workflow directly.
3. Diagnosis content quality is acceptable-to-good, but init-failure scenarios (`golden_17`, `golden_18`) regressed because the new vague-failure clarification behavior is over-applied to explicit test prompts.
4. Scheduler and reboot/reset-password flows are still not sign-off quality on this model.

## Artifacts
- [live_acceptance_golden_output.txt](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round6-doubao-code\live_acceptance_golden_output.txt)
- [live_acceptance_eval_output.txt](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round6-doubao-code\live_acceptance_eval_output.txt)