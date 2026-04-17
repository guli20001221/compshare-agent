# Real Account Shadow QA

Date: 2026-04-16
Scope: Block A real-account shadow QA against the logged-in 优云控制台 account.

Rules:
- Only operate on instances created during this QA run.
- Never modify or stop pre-existing instances.
- Store screenshots, transcripts, and result summaries under `artifacts/`.
- Keep this folder separate from `eval/golden*` and mock/live golden assets.

Planned checks:
1. Read current console state and record pre-existing instances.
2. Create one or more disposable test instances for write-path validation.
3. Exercise CLI agent against real account state.
4. Compare agent behavior with actual control-panel state and record mismatches.
