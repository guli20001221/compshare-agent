# Agent Terminology Glossary

This is the **canonical naming source of truth** for the CompShare console agent.
It exists because the codebase carries naming from its fast-iteration period that
is not aligned with the industrialized agent vocabulary used by OpenAI and
Anthropic. New code and new docs MUST use the canonical names here; legacy names
are being migrated (see [Migration policy](#migration-policy)).

> **P6 update.** The central-Agent cutover (P6) **physically deleted** the intent
> router (`internal/intent/planner.go`), `internal/orchestrator`, and
> `internal/router` — the central AgentRuntime is now the only production path.
> Section 1 (industry vocabulary) and Section 3 (migration policy) stay current; in
> Section 2, the rows that describe the intent router / `Saga` orchestrator /
> `internal/router` as code to rename are **historical naming-debt records**, not
> live migration targets (the code is gone). Current architecture:
> [`architecture.md`](architecture.md).

Primary sources:
- OpenAI Agents SDK — Agent / Tool / Guardrail / Handoff / Run: https://developers.openai.com/api/docs/guides/agents
- OpenAI Agents SDK Tracing — Trace / Span: https://openai.github.io/openai-agents-python/tracing/
- Anthropic, *Building Effective Agents* — Workflow vs Agent, routing / orchestrator-workers / evaluator-optimizer: https://www.anthropic.com/engineering/building-effective-agents
- Anthropic, *Writing effective tools for agents* — tool naming & namespacing: https://www.anthropic.com/engineering/writing-tools-for-agents
- Claude context engineering cookbook — Context / Memory / Compaction: https://platform.claude.com/cookbook/tool-use-context-engineering-context-engineering-tools

## 1. Canonical agent vocabulary (industry-standard)

| Term | Definition (as we use it) |
|---|---|
| **Agent** | An LLM-driven executor that dynamically decides its own path/steps — plans, calls tools, keeps state, completes multi-step tasks. In this repo: the ReAct loop. |
| **Workflow** | A fixed or semi-fixed code path over LLM + tools. NOT an agent. Anthropic's named patterns: **routing** (classify → dispatch), **orchestrator-workers**, **evaluator-optimizer**, prompt-chaining, parallelization. |
| **Routing** | The workflow pattern that classifies an input and dispatches to a fixed handler. Our **pre-P6** intent layer was exactly this; P6 removed the standalone classification step — the central Agent now decides in-loop. |
| **Tool** | A typed, named capability the agent invokes; the result is fed back into context. Tools should have clear names, boundaries, and namespacing. |
| **Guardrail** | A code-enforced safety rule (destructive-action block, mutating-action confirmation, tool-policy check, citation/leak validation). NOT a model judgment. |
| **Trace / Span** | A trace is one end-to-end run; spans are nested timed sub-operations within it. Production-observability vocabulary. |
| **Handoff** | One agent transferring control to another. |
| **Run** | One execution of an agent/loop over an input. |
| **Context** | The working window assembled for a model call. |
| **Memory** | Durable context preserved across runs (facts, decisions, verification results). NOT a rate limiter, NOT a cache. |
| **Compaction** | Summarizing/condensing long context when it exceeds the window. |
| **MCP** | A transport protocol (JSON-RPC) for **exposing** tools/resources/prompts (server) or **consuming** them (client). NOT a tool, agent, skill, or "gateway". |

## 2. This project — legacy → canonical names

Verified against `main` (file:line where it helps). "Why" cites the mismatch.

### Intent layer (it is a router, not a planner)
| Legacy | Canonical | Why |
|---|---|---|
| `Planner` / `Plan` (aliases) | `IntentRouter` / `IntentRoute` | One LLM call → one intent + slots → dispatch. Classification + routing, not multi-step planning. (`internal/intent/planner.go:83` already has `IntentRouter`; `:100`/`types.go` keep deprecated aliases.) |
| `IntentRouter.Plan()` method | `IntentRouter.Route()` | The method classifies/routes; it does not produce a plan. |
| `CompleteIntentPlan` (LLM iface) | `CompleteIntentRoute` / `ClassifyIntent` | Sounds like task-plan generation; it is classification. (`planner.go:24`) |
| `PlannerTrace` | `IntentRoutingTrace` | Records the routing decision, not a plan. |
| `PlannerInput` / `PlannerResult` / `PlannerOptions` / `PlannerLLM` | `IntentRouter{Input,Result,Options,LLM}` | Consistency with `IntentRouter`. |
| `cutover` / `Phase-1 cutover` | `direct_dispatch` / `deterministic_dispatch` | A migration-phase label, not a durable architecture term. (`CLAUDE.md`, `json:"cutover_status"`/`"cutover_intents"`.) |

### The three "routers" — disambiguate
| Legacy | Canonical | Why |
|---|---|---|
| `internal/router` (package) | `internal/inputguard` (or `preblock`) | It is `PreBlock`, "a thin, domain-agnostic rule dispatcher" (`internal/router/preblock.go:3`) — input guarding, not business routing. |
| ~~`internal/routing` (package)~~ | *(removed)* | The route.yaml catalog/registry metadata and its dispatch stack (`cmd/routegen`, `DispatchRoute`) were physically deleted in P6 — read dispatch now runs through the typed Capability catalog (`internal/capability`). |
| `llm.Router` | `ModelRouter` | Tier-aware **model** selection; collides with the intent router. (`internal/llm/router.go:44`) |

### Other over-promising / unclear names
| Legacy | Canonical | Why |
|---|---|---|
| `Saga` (orchestrator) | `StepRunner` / `WorkflowRunner` | The code itself states it does **not** implement reverse-compensation; `Step.Compensate` is "reserved-but-unconsumed" (`internal/orchestrator/saga.go:22-23`). "Saga" over-promises durable compensation we don't have. |
| `MemoryLimiter` | `InMemoryRateLimiter` | Implements `RateLimiter` (`internal/governance/ratelimit.go:48/98`) — in-memory rate limiting, unrelated to agent memory. |
| `agentic SearchKnowledge` | `KnowledgeSearchTool` | "agentic" is too generic; it's a read-only retrieval **tool**. |
| `runtime_form` | `execution_path` / `execution_mode` | Non-standard term; reads poorly externally. |
| `RealizedTier` | `ActualExecutionTier` | Disambiguate from `TaskTier` / `RuntimeForm`. |
| `ConfirmBroker` | `UserConfirmationBroker` | Make the human-in-the-loop subject explicit. |
| `CSAgent` | `CompShareAgent` | Avoid an unfriendly external abbreviation. **DEFERRED (won't-fix for the byte-stable sweep):** `CSAgent` survives ONLY as HTTP **Action wire values** the frontend posts and `dispatch.go`/`ws.go` route on (`SendCSAgentChat`, `ConfirmCSAgentAction`, `GetCSAgentMeta`, `CreateCSAgentSession`, `GetCSAgentSession`, `SendCSAgentFeedback`, `CreateCSAgentWS`) — there are **no internal Go symbols**. Renaming the strings breaks the deployed frontend, so it needs a coordinated server+frontend change or a dual-route compat window. Revisit with a planned API v2. |
| `capability` (non-LLM senses) | reserve only for `ModelCapability` | Historically meant skill/route-abstraction; now should mean only model capability (`SupportsObjectToolChoice`, …). The route-abstraction sense was retired (#115). |
| `KQA` | `KnowledgeQA` | Over-abbreviated; opaque outside the team. |
| Env: `USE_INTENT_PLANNER`, `COMPSHARE_INTENT_ROUTER_MODE` | retired | The independent intent-router runtime was removed when the central AgentRuntime became the only production path. |
| Env: `USE_INTENT_PLANNER_FOR`, `COMPSHARE_DIRECT_DISPATCH_INTENTS` | retired | Direct business dispatch no longer selects the production semantic path. |
| Env: `USE_*` / `RAG_*` / `PLANNER_*` mix | `COMPSHARE_*` | One config prefix. |

**Kept as-is (industry-standard already):** `RAG` (internal use is fine — it's the standard acronym; spell out as "knowledge retrieval" only in user-facing product copy), `Tool`, `Guardrail`, `Trace`, `Agent`, `Workflow`, `MCP`.

## 3. Migration policy

Naming changes follow the same risk discipline as any change here:

1. **This glossary first** — the canonical names are defined here before code moves.
2. **New code uses canonical names only.** Don't add new `Planner`/`cutover`/`Saga`-style names.
3. **Internal Go symbols + filenames** rename freely (byte-stable — the rendered prompts don't change; the planner/router-prompt SHA pin proves it). Keep a deprecated alias for one release where external-ish call sites exist.
4. **Prompt text** that the model reads (e.g. "You are the … planner") is a behavior surface — rename behind an eval-gated A/B, not byte-stable.
5. **Wire/ops contracts are NOT free to rename**: the trace JSON tags (`json:"planner"`, `json:"cutover_status"`, `json:"cutover_intents"`) and env flags (`USE_INTENT_PLANNER*`) are consumed by trace storage/dashboards **and the eval harness itself**. These move behind a compat window (dual-emit / dual-read) with consumers updated in the same change.

Priority order (highest naming-debt first): **Planner → IntentRouter**, **cutover → direct dispatch**, the **three routers** split, then env-prefix unification, then the remaining over-promising names (`Saga`, `MemoryLimiter`, …).
