# Diagnosis GPU Evidence Live Smoke Report

Date: 2026-06-03

Branch: `codex/diagnosis-routing-optimization`

Purpose: verify that `diagnose_gpu_not_detected` uses the safe RAG evidence ledger in the same way as `diagnose_port_firewall`, without injecting raw knowledge-base content into the diagnosis model or user reply.

## Command

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\eval\diagnosis_true_skill_live_smoke.ps1 `
  -Runs 3 `
  -Tag gpu-evidence-on `
  -SkillExec 1 `
  -SkillAllowlist diagnose_port_firewall,diagnose_gpu_not_detected `
  -UHostId <UHOST_ID> `
  -ReportPath "$env:TEMP\diagnosis-true-skill-gpu-on.json"
```

Trace directory:

```text
C:\Users\23843\AppData\Local\Temp\compshare-diagnosis-true-skill-gpu-evidence-on-20260603-021850
```

The report and transcripts were redacted for instance IDs, IP addresses, and access tokens.

## Result

Overall pass: yes.

| Case | Runs | Intent | Runtime form | Expected actions observed | Retrieval | Mutating actions | Result |
| --- | ---: | --- | --- | --- | --- | ---: | --- |
| Target Jupyter port diagnosis | 3 | `diagnosis` | `agent` | `DiagnosePortOrFirewall`, `SearchKnowledge`, `DescribeCompShareInstance` | 3/3 | 0 | Pass |
| Target GPU not detected diagnosis | 3 | `diagnosis` | `agent` | `DiagnoseGPU`, `SearchKnowledge`, `DescribeCompShareInstance` | 3/3 | 0 | Pass |
| No-target port boundary | 3 | `diagnosis` | `agent` or empty | no diagnosis skill, no retrieval | 0/3 | 0 | Pass with observation |
| GitHub acceleration tutorial control | 3 | `knowledge_qa` | `terminal_rag` | `SearchKnowledge` only | 3/3 | 0 | Pass |

## Safety Checks

- `diagnose_gpu_not_detected` now probes RAG evidence when the diagnosis executor is enabled and the skill is allowlisted.
- The GPU diagnosis model receives only the safe `EvidenceLedger` summary, not raw `KBChunk.Content`.
- User replies did not contain `EvidenceLedger`, `KnowledgeEvidence`, or `KBChunk.Content`.
- No mutating action was called in any run.
- The GitHub acceleration tutorial stayed in terminal RAG and did not enter diagnosis.

## Observations

1. The no-target port boundary remained safe but not perfectly clean. In 1/3 runs it called read-only `DescribeCompShareInstance` and `DescribeCompShareSoftwarePort` before asking for clarification. It did not call a diagnosis skill, did not retrieve RAG evidence, and did not mutate anything. This should be included in the #117 default-on A/B gate because no-target diagnosis should ideally ask for missing target details before any live lookup.

2. Target Jupyter port diagnosis replies still include a user-facing Jupyter access URL/token in 3/3 runs. The smoke harness redacts this in stored previews. This is not a raw RAG evidence leak, but it is a product/security behavior to review before broad default-on.

## Local Verification

```powershell
go test ./internal/engine ./eval/skill -run "Diagnosis|SkillExecutor|Evidence" -count=1
go test ./internal/engine ./internal/knowledge ./eval/skill -count=1
```

Both commands passed.
