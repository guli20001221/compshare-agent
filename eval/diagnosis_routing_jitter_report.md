# Diagnosis Routing Jitter Report

Date: 2026-06-03

Branch: `codex/diagnosis-routing-optimization`

Purpose: verify the narrow #123 routing fix. Runtime failure reports without an explicit instance target should route to `diagnosis`; pure tutorial, configuration, and error-code questions should stay `knowledge_qa`.

## Environment

- Real CLI: `agent.exe cli -c .\deploy\conf\agent.yaml`
- Project: `org-cwy2qk`
- Mutating tools: disabled
- Skill executor: disabled
- Runs per question: 5
- Report script: `eval/diagnosis_routing_jitter.ps1`
- Case file: `eval/diagnosis_routing_jitter_questions.json`

## Baseline

Source: `%TEMP%\diagnosis-routing-baseline.json`

Overall: failed.

| Case | Expected | Diagnosis | Knowledge QA | Result |
| --- | --- | ---: | ---: | --- |
| port_why | diagnosis | 0/5 | 5/5 | fail |
| gpu_notfound | diagnosis | 0/5 | 5/5 | fail |
| ssh_timeout | diagnosis | 3/5 | 2/5 | fail |
| port_external | diagnosis | 4/5 | 1/5 | pass |
| init_stuck | diagnosis | 5/5 | 0/5 | pass |
| tutorial_github | knowledge_qa | 0/5 | 5/5 | pass |
| slow_download | knowledge_qa | 0/5 | 5/5 | pass |
| platform_howto | knowledge_qa | 0/5 | 5/5 | pass |
| error_code_howto | knowledge_qa | 0/5 | 5/5 | pass |

Baseline trace dir: `C:\Users\23843\AppData\Local\Temp\compshare-diagnosis-routing-baseline-20260603-012730`

## After Fix

Source: `%TEMP%\diagnosis-routing-after.json`

Overall: passed.

| Case | Expected | Diagnosis | Knowledge QA | Planned form | Cutover | Result |
| --- | --- | ---: | ---: | --- | --- | --- |
| port_why | diagnosis | 5/5 | 0/5 | agent 5/5 | fallback_ineligible 5/5 | pass |
| gpu_notfound | diagnosis | 5/5 | 0/5 | agent 5/5 | fallback_ineligible 5/5 | pass |
| ssh_timeout | diagnosis | 5/5 | 0/5 | agent 5/5 | fallback_ineligible 5/5 | pass |
| port_external | diagnosis | 5/5 | 0/5 | agent 5/5 | fallback_ineligible 5/5 | pass |
| init_stuck | diagnosis | 5/5 | 0/5 | agent 5/5 | fallback_ineligible 5/5 | pass |
| tutorial_github | knowledge_qa | 0/5 | 5/5 | terminal_rag 5/5 | dispatched_retrieval 5/5 | pass |
| slow_download | knowledge_qa | 0/5 | 5/5 | terminal_rag 5/5 | dispatched_retrieval 5/5 | pass |
| platform_howto | knowledge_qa | 0/5 | 5/5 | terminal_rag 5/5 | dispatched_retrieval 5/5 | pass |
| error_code_howto | knowledge_qa | 0/5 | 5/5 | terminal_rag 5/5 | dispatched_retrieval 5/5 | pass |

After trace dir: `C:\Users\23843\AppData\Local\Temp\compshare-diagnosis-routing-after-20260603-014126`

## Commands

```powershell
$env:COMPSHARE_PROJECT_ID='org-cwy2qk'
go test ./cmd ./internal/intent ./internal/engine ./internal/knowledge ./eval/skill -count=1
go vet ./cmd ./internal/intent ./internal/engine ./internal/knowledge ./eval/skill
go build -o agent.exe ./cmd
powershell -NoProfile -ExecutionPolicy Bypass -File .\eval\diagnosis_routing_jitter.ps1 -Runs 5 -Tag after -ReportPath "$env:TEMP\diagnosis-routing-after.json"
```

## Notes

- This phase intentionally changes the planner prompt and updates `systemPromptSHA256Baseline`.
- The fix is scoped to action-shape classification for runtime failure reports.
- The report does not include raw transcripts, secrets, or live account identifiers beyond the test project name already used by prior smokes.
