# PR2 router A/B report

## Summary

The PR2 prompt-slimming check was run with the live `deepseek-v4-flash` router. The full online intent fixture is naturally jittery, so the result is treated as an A/B comparison instead of a hard absolute threshold.

## Runs

Baseline arm: pre-PR2 commit `1d26fe24` in `F:\compshare-agent\.worktrees\pr2-ab-baseline`.

- Run 1: `59/68` intent accuracy (`86.8%`)
- Run 2: `59/68` intent accuracy (`86.8%`)

Current arm: PR3 branch `codex/pr3-retrieval-gates`.

- Run 1: `60/68` intent accuracy (`88.2%`), failed only the earlier experimental 90% absolute threshold.
- Run 2: `58/68` intent accuracy (`85.3%`)

## Interpretation

The live model jitters by roughly the same size as the observed branch delta. The slimmed PR2 prompt did not show a clear regression against the pre-PR2 baseline in this fixture.

The repeated misses are mostly existing ambiguous or legacy labels such as account-billing unsupported, vague failure, unknown operations, and mixed unknowns. They are not new create/deploy paid-action regressions.

## Evidence files

- `router_online_eval_baseline_pre_pr2.log`
- `router_online_eval_baseline_pre_pr2_run2.log`
- `router_online_eval.log`
- `router_online_eval_current.log`

## Notes

This fixture does not replace the behavioral gate for create/deploy confirmation behavior. PR3's separate live smoke verifies the knowledge-retrieval facts, and earlier R2b Gate-1 verified create/deploy/tool behavior.

