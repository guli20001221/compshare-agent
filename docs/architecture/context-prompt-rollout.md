# Historical Context and Prompt Rollout

Date: 2026-06-03 (superseded)

This document previously described the Planner, skill-executor, session-fact,
and history-compaction rollout. Those mechanisms no longer exist in the runtime.
Do not enable or reintroduce their old environment variables.

The current design is deliberately smaller:

- the central Agent decides which ordinary tools to call;
- canonical transcript replay is the semantic history for tool turns;
- token budgets bound assembled requests and replayed history;
- result projection may reduce oversized read results before the next model call;
- persisted state is limited to execution workflow state, selection authority,
  and evidence used by the knowledge verifier.

Current operating guidance lives in [architecture.md](../architecture.md) and
the runtime flag table in `CLAUDE.md`. The original rollout evidence remains in
Git history rather than as runnable instructions for removed flags.
