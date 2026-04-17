# Live Acceptance Summary (Doubao-Seed-Mini)

Date: 2026-04-17
Model: `doubao-seed-2-0-mini-260215`

## Commands
- `go test ./... -count=1`
- `go test -count=1 -timeout 45m ./eval -run TestGoldenScripts -v -args -model "Doubao-Seed-Mini"`
- `go test -count=1 -timeout 45m ./eval -run TestEval -v -args -model "Doubao-Seed-Mini"`

## Result
- Local regression: `go test ./...` PASS
- Golden live acceptance: **14/25 PASS**, **11/25 FAIL**
- Eval live acceptance: **intent 69.7% (53/76)** / **tool 59.2% (29/49)** / **content 77.4% (24/31)**

## Key observations
1. Mini regressed heavily on workflow-first routing for write actions. In many golden cases it issued a leading `DescribeCompShareInstance` or stayed in natural-language confirmation instead of entering the workflow directly.
2. Scheduler and reboot/reset-password style operations regressed in the same way: route purity and tool-choice were significantly weaker than the Lite baseline.
3. `TestEval` is materially below the Block A exit target and also below the Lite baseline (`92.1 / 93.9 / 81.2`).
4. Mini is therefore not a drop-in cheaper substitute for final acceptance. It is useful for low-cost smoke testing, but not for sign-off.

## Artifacts
- [live_acceptance_golden_output.txt](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round4-mini-freeze\live_acceptance_golden_output.txt)
- [live_acceptance_eval_output.txt](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round4-mini-freeze\live_acceptance_eval_output.txt)