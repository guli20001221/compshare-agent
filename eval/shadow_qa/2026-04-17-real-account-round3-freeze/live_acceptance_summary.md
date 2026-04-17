# Live Acceptance Summary

Date: 2026-04-17
Model: `Doubao-Seed-Lite`
Note: future low-cost follow-up runs should switch to `Doubao-Seed-Mini`; this round remains the Lite baseline because it was already in flight.

Artifacts:
- [live_acceptance_eval_output.txt](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round3-freeze\live_acceptance_eval_output.txt)
- [live_acceptance_golden_output.txt](F:\compshare-agent\eval\shadow_qa\2026-04-17-real-account-round3-freeze\live_acceptance_golden_output.txt)

## 1. Offline Eval (`TestEval`)
Command:

```powershell
$env:DOUBAO_API_KEY='***'
go test -count=1 -timeout 45m ./eval -run TestEval -v -args -model 'Doubao-Seed-Lite'
```

Result:
- Intent accuracy: **92.1%** (70/76)
- Tool accuracy: **93.9%** (46/49)
- Content accuracy: **81.2%** (26/32)
- Duration: **7m47s**

Threshold comparison:
- Intent target `>= 95%`: **FAIL**
- Tool target `>= 95%`: **FAIL**
- Content target `>= 90%`: **FAIL**

Most important misses:
- `sq_03`: expected `CheckCompShareResourceCapacity`, got `DescribeCompShareImages`
- `ct_03`: expected `StartInstanceWorkflow`, got `DescribeCompShareInstance`
- `ct_10`: expected `ResetPasswordWorkflow`, got text-only knowledge response
- `ct_13/14/15/16`: scheduler workflows were classified as `simple_query` because `toolToIntent()` still does not map scheduler workflows into `complex_task`
- `dg_02/dg_04/dg_06/dg_06b/dg_07`: diagnosis content keywords were too weakly hit
- `kq_04`: only 1/3 expected content keywords hit

Interpretation:
- Block A **does not yet pass** the main-model eval thresholds on Doubao-Seed-Lite.
- Some misses are model quality / content issues.
- Some misses are evaluation-framework issues (notably scheduler intent classification in `toolToIntent()`).

## 2. Engine Golden (`TestGoldenScripts`)
Command:

```powershell
$env:DOUBAO_API_KEY='***'
go test -count=1 -timeout 45m ./eval -run TestGoldenScripts -v -args -model 'Doubao-Seed-Lite'
```

Result:
- **25/25 PASS**

Interpretation:
- Structured golden coverage is currently strong.
- The remaining Block A exit problem is not golden breakage; it is the stricter `TestEval` threshold gap plus the remaining real-user dirty-routing issues.

## Final judgment
- **Golden acceptance: PASS**
- **Main-model eval thresholds: FAIL**
- Recommended next test model for follow-up cost-sensitive reruns: **Doubao-Seed-Mini**

