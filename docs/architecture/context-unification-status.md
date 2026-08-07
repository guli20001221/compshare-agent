# Context Unification Status

Last audited against `origin/main` at `e716f0be`.

## Runtime model

The central Agent receives the current user message, normal chat history, and
canonical replay of prior tool calls/results when transcript replay is enabled.
It decides whether to answer, query, or propose an operation. There is no
intent router, task planner, model-side workflow classifier, or server-side
referent substitution layer ahead of that loop.

Context is bounded by token/rune budgets, not by a fixed number of remembered
turns. The assembler preserves complete user/assistant exchanges and complete
tool-call groups; it sheds oldest history before current-turn tool groups while
keeping the system prompt and current user message.

## What is deliberately not semantic memory

The following former replicas are deleted and never re-enter model context:

- `ContextFrame`, `TaskSnapshot`, and `ConversationDigest`;
- `RecentFacts` (including stock/GPU carry and expiry metadata);
- `VerifiedKnowledge` answer text and `UpdateTaskState`.

`VerifiedKnowledge` retains source evidence only for the answer verifier; it
is not injected into the Agent prompt.

## State that remains on purpose

The remaining session state is workflow or authorization state, not a second
semantic transcript:

- selected-instance provenance and TTL;
- the bounded candidate list behind an explicit selection card;
- incomplete creation form / confirmation state, idempotency, and write-time
  live revalidation;
- read-only continuity notices supplied by the coordinator.

The selection binder uses the structured state directly. Rendering or removing
the Agent context card cannot authorize a write target.

## Deliberate limits

`react_result_projection` remains because it bounds large list/read outputs
within a turn; it does not summarize cross-turn history. The canonical
transcript switch is a boot-time migration switch: false stops capture,
persistence, and replay together, but does not restore removed semantic state.

Durable turns remain disabled. Their historical read boundary is intentionally
kept separate from the active transcript path until durable operation is a
separate product decision.
