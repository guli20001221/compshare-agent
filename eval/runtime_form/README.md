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
| planned runtime form accuracy | pending planned-runtime trace producer | pending |
| planned/actual mismatch rate | pending planned-runtime trace producer | pending |

The pending rows are intentional. They must not be faked from the actual side:
planned-vs-actual mismatch is meaningful only after the planner-side
`planned_runtime_form` trace exists in the same merged code path.
