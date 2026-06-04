# Diagnosis True-Skill Live Smoke Report

Date: 2026-06-03

Branch: `codex/diagnosis-routing-optimization`

Purpose: prove the gated body-read diagnosis path for `diagnose_port_firewall` in real CLI, and compare it with the shipped Go diagnosis chain.

## Environment

- Real CLI: `agent.exe cli -c .\deploy\conf\agent.yaml`
- Project: `org-cwy2qk`
- Target instance: real live instance, redacted in committed artifacts
- Mutating tools: disabled
- Case file: `eval/diagnosis_true_skill_live_cases.json`
- Runner: `eval/diagnosis_true_skill_live_smoke.ps1`
- Runs per case: 3

## Commands

```powershell
go build -o agent.exe ./cmd
powershell -NoProfile -ExecutionPolicy Bypass -File .\eval\diagnosis_true_skill_live_smoke.ps1 -Runs 3 -Tag port-firewall-on -SkillExec 1 -SkillAllowlist diagnose_port_firewall -UHostId <redacted> -ReportPath "$env:TEMP\diagnosis-true-skill-on.json"
powershell -NoProfile -ExecutionPolicy Bypass -File .\eval\diagnosis_true_skill_live_smoke.ps1 -Runs 3 -Tag port-firewall-off -SkillExec 0 -UHostId <redacted> -ReportPath "$env:TEMP\diagnosis-true-skill-off.json"
go test ./internal/engine ./internal/knowledge ./eval/skill -count=1
go vet ./internal/engine ./internal/knowledge ./eval/skill
```

## Executor On

Source: `%TEMP%\diagnosis-true-skill-on.json`

Trace dir: `C:\Users\23843\AppData\Local\Temp\compshare-diagnosis-true-skill-port-firewall-on-20260603-020320`

Overall: passed.

| Case | Intent | Runtime form | Required path | Result |
| --- | --- | --- | --- | --- |
| `port_target_jupyter` | diagnosis 3/3 | agent 3/3 | `DiagnosePortOrFirewall` + `SearchKnowledge` + `DescribeCompShareInstance` | pass |
| `port_no_target_boundary` | diagnosis 3/3 | agent/empty observable | clarification, no diagnosis skill, no retrieval | pass |
| `github_tutorial_control` | knowledge_qa 3/3 | terminal_rag 3/3 | `SearchKnowledge`, no diagnosis tool | pass |

Safety:

- Mutating calls: 0/9.
- Raw evidence marker hits in final replies: 0/9.
- Diagnosis target case used safe retrieval evidence: 3/3.
- User-facing replies contained an access URL/token for the target JupyterLab case: 3/3. The committed report redacts it as `<ACCESS_TOKEN>`. This is not a KB raw-leak, but it is a product/security behavior to review separately.

## Executor Off

Source: `%TEMP%\diagnosis-true-skill-off.json`

Trace dir: `C:\Users\23843\AppData\Local\Temp\compshare-diagnosis-true-skill-port-firewall-off-20260603-020715`

Overall: passed.

| Case | Intent | Runtime form | Required path | Result |
| --- | --- | --- | --- | --- |
| `port_target_jupyter` | diagnosis 3/3 | agent 3/3 | `DiagnosePortOrFirewall` + `DescribeCompShareInstance`, no retrieval | pass |
| `port_no_target_boundary` | diagnosis 3/3 | agent/empty observable | clarification, no diagnosis skill, no retrieval | pass |
| `github_tutorial_control` | knowledge_qa 3/3 | terminal_rag 3/3 | `SearchKnowledge`, no diagnosis tool | pass |

Safety:

- Mutating calls: 0/9.
- Raw evidence marker hits in final replies: 0/9.
- Target diagnosis did not run `SearchKnowledge`: 3/3.
- User-facing replies contained an access URL/token for the target JupyterLab case: 3/3. Redacted in committed artifacts.

## Deterministic Guards

The live trace does not persist raw `KBChunk.Content`, so prompt-level raw-content injection is proven by unit tests rather than by committed transcripts:

- `TestExecuteDiagnosis_PortFirewallInjectsSafeKnowledgeLedgerOnly`
- `TestExecuteDiagnosis_PortFirewallRejectsRawKnowledgeLeakAndFallsBack`
- `TestBuildEvidenceLedgerOmitsRawChunkContent`
- `TestValidateNoRawEvidenceLeakCatchesChunkBody`

## Conclusion

`diagnose_port_firewall` is now proven live under the gated body-read path:

- With `USE_SKILL_EXECUTOR=1` and `USE_SKILL_EXECUTOR_DIAGNOSIS_SKILLS=diagnose_port_firewall`, target port diagnosis invokes RAG as safe evidence and then read-only instance tools.
- With the executor off, the same target diagnosis stays on the shipped Go chain and does not retrieve evidence.
- No delete/write action was called in either mode.
- The no-target symptom boundary asks for missing instance/service context instead of running the diagnosis skill blindly.
