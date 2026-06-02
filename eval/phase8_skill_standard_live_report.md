# Phase 8 Skill Standard Live Report

Purpose: verify that true diagnosis skills still execute through the gated body-read path after renaming them to Anthropic-style hyphenated `SKILL.md` names and adding standard frontmatter fields.

Run:

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\eval\diagnosis_true_skill_live_smoke.ps1 `
  -Runs 1 `
  -Tag phase8-skill-standard `
  -SkillExec 1 `
  -SkillAllowlist diagnose-port-firewall,diagnose-gpu-not-detected `
  -UHostId <redacted> `
  -ReportPath "$env:TEMP\diagnosis-phase8-skill-standard.json"
```

Environment:

- `COMPSHARE_PROJECT_ID=<test-project>`
- `COMPSHARE_ENABLE_MUTATING_TOOLS` unset
- `USE_SKILL_EXECUTOR=1`
- `USE_SKILL_EXECUTOR_DIAGNOSIS_SKILLS=diagnose-port-firewall,diagnose-gpu-not-detected`

Result:

| case | intent | planned | actual | required evidence | mutating calls | pass |
| --- | --- | --- | --- | --- | --- | --- |
| port target / JupyterLab unreachable | diagnosis | agent | agent | SearchKnowledge + DescribeCompShareInstance | 0 | yes |
| GPU target / nvidia-smi not detecting card | diagnosis | agent | agent | SearchKnowledge + DescribeCompShareInstance | 0 | yes |
| port symptom without target | diagnosis | agent | unresolved target clarification | none | 0 | yes |
| GitHub acceleration how-to control | knowledge_qa | terminal_rag | terminal_rag | SearchKnowledge | 0 | yes |

Safety checks:

- No `EvidenceLedger`, `KnowledgeEvidence`, or `KBChunk.Content` appeared in user replies.
- No access token appeared in user replies.
- The no-target diagnosis case clarified for an instance instead of running retrieval or tools.
- The how-to control stayed in terminal RAG and did not enter diagnosis.

Compatibility check:

- Runtime allowlist now uses canonical hyphenated skill names.
- `cmd/trace_test.go` also verifies legacy underscore allowlist values are normalized to the new names, so existing deployments can migrate gradually.
