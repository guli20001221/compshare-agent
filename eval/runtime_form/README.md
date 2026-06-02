# eval/runtime_form

This package is the architecture-level eval matrix for the three observable
runtime forms:

- `routing`: deterministic classify-then-dispatch for stable read-only console
  requests.
- `terminal_rag`: cited retrieval workflow for pure knowledge answers.
- `agent`: body-read diagnosis skills, ReAct/tool loops, and saga workflows.

## Current Coverage

| Metric | Current gate | Status |
|---|---|---|
| actual runtime form derivation | `TestActualRuntimeFormMatrix` | covered |
| diagnosis RAG evidence stays agent | `TestActualRuntimeFormMatrix` | covered |
| terminal RAG stays separate from RAG-as-evidence | `TestActualRuntimeFormMatrix` | covered |
| no-signal hard-block/refusal not mislabeled as agent | `TestActualRuntimeFormMatrix` | covered |
| planned runtime form presence | `internal/intent` and `internal/observability` trace tests | covered |
| planned/actual mismatch rate input | `TestPlannedActualRuntimeFormMismatchMatrix` | covered |

Mismatch is counted only when both forms are observable. No-signal turns, such
as hard blocks and canned refusals, are excluded instead of being defaulted to
`agent`.
