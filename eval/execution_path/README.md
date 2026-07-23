# eval/execution_path

> **LEGACY TRACE COMPAT — not the current architecture.** The `routing` /
> `terminal_rag` / `agent` runtime-form taxonomy below predates the P6
> central-Agent cutover. The current runtime has a **single** execution form (the
> central Agent loop, `internal/engine`). These tests are retained ONLY to guard
> that `observability.TraceRecord.DeriveActualExecutionPath` /
> `ExecutionPathMismatch` still classify **historical and cutover-era** trace
> records correctly, so trace storage / dashboards / the eval harness keep working
> across the schema migration. Do **not** read this package as a description of how
> the system routes today — see `docs/architecture.md`. P7 acceptance must judge a
> turn by its real tools / steps / observations / final answer, not by a derived
> form label.

This package pins the derivation of the retired runtime-form labels:

- `routing`: legacy deterministic classify-then-dispatch label.
- `terminal_rag`: legacy cited-retrieval-workflow label.
- `agent`: legacy label for body-read diagnosis, ReAct/tool loops, and saga workflows.

## Coverage (legacy-derivation guards)

| Metric | Gate | Status |
|---|---|---|
| legacy form derivation from a trace | `TestActualExecutionPathMatrix` | covered |
| diagnosis RAG evidence derives to `agent`, not `terminal_rag` | `TestActualExecutionPathMatrix` | covered |
| no-signal hard-block/refusal not mislabeled `agent` | `TestActualExecutionPathMatrix` | covered |
| planned/actual legacy-form mismatch input | `TestPlannedActualExecutionPathMismatchMatrix` | covered |

Mismatch is counted only when both legacy forms are observable. No-signal turns
(hard blocks, canned refusals) are excluded rather than defaulted to `agent`.
