# eval/execution_path

This package is the architecture-level eval matrix for the three observable
runtime forms:

- `routing`: deterministic classify-then-dispatch for stable read-only console
  requests.
- `terminal_rag`: cited retrieval workflow for pure knowledge answers.
- `agent`: body-read diagnosis skills, ReAct/tool loops, and saga workflows.

## Current Coverage

| Metric | Current gate | Status |
|---|---|---|
| actual runtime form derivation | `TestActualExecutionPathMatrix` | covered |
| diagnosis RAG evidence stays agent | `TestActualExecutionPathMatrix` | covered |
| terminal RAG stays separate from RAG-as-evidence | `TestActualExecutionPathMatrix` | covered |
| no-signal hard-block/refusal not mislabeled as agent | `TestActualExecutionPathMatrix` | covered |
| planned runtime form presence | `internal/intent` and `internal/observability` trace tests | covered |
| planned/actual mismatch rate input | `TestPlannedActualExecutionPathMismatchMatrix` | covered |

Mismatch is counted only when both forms are observable. No-signal turns, such
as hard blocks and canned refusals, are excluded instead of being defaulted to
`agent`.
