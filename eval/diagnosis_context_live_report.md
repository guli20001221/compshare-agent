# Diagnosis Structured Context Live Spot Report

Date: 2026-06-03

Branch: `codex/diagnosis-routing-optimization`

Purpose: verify that structured diagnosis seed context does not break the live body-read diagnosis path.

## Command

```powershell
go build -o agent.exe ./cmd
powershell -NoProfile -ExecutionPolicy Bypass -File .\eval\diagnosis_true_skill_live_smoke.ps1 `
  -Runs 1 `
  -Tag structured-context `
  -SkillAllowlist diagnose_port_firewall,diagnose_gpu_not_detected `
  -UHostId <UHOST_ID> `
  -ReportPath "$env:TEMP\diagnosis-true-skill-structured-context.json"
```

Trace directory:

```text
C:\Users\23843\AppData\Local\Temp\compshare-diagnosis-true-skill-structured-context-20260603-044512
```

## Result

Overall: **pass**.

| Case | Result |
| --- | --- |
| `port_target_jupyter` | pass |
| `gpu_target_nvidia_smi` | pass |
| `port_no_target_boundary` | pass |
| `github_tutorial_control` | pass |

## Safety Checks

- Port and GPU target cases used the body-read diagnosis path.
- RAG evidence stayed as safe ledger content; final replies did not contain `EvidenceLedger`, `KnowledgeEvidence`, or `KBChunk.Content`.
- No mutating tool calls occurred.
- No access-token-like reply content was emitted.
- No-target diagnosis asked for a target before live lookup.
- GitHub tutorial stayed in terminal RAG.
