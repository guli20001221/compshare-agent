# Agent Runtime Terms

This document defines the runtime terms used by the Youyun CompShare console
agent. Its purpose is to keep the current primitives — the central Agent loop,
typed read capabilities, the action resolver, sealed workflows, typed
observations, the response gateway, RAG-as-a-tool, guardrails, and MCP —
separate in both code and review language.

> **History.** An earlier design split execution into three runtime *forms*
> (`routing` / `terminal_rag` / `agent`), used a Planner to predict a
> `planned_execution_path`, and carried model-read *true skills* with progressive
> disclosure under `internal/skills`. That whole stack — `internal/routing`,
> `cmd/routegen`, route manifests, `internal/skills`, `cmd/skillgen`, the
> terminal-RAG prompt — was **physically deleted in P6**. There is now a single
> central Agent loop; the terms below describe what actually runs. Retired terms
> (`routing`, `terminal_rag`, `true skill`, `planned_execution_path`) should be
> used only when discussing history, and marked as such.

## Terms

### Tool

A tool is a typed callable action. It has a name, input schema, policy level, and
execution result. Console API wrappers (instance description, pricing, stock,
image listing) and the mutating API calls are tools; `SearchKnowledge` is the
read-only knowledge-retrieval tool.

Tools are not playbooks. The central Agent or a deterministic workflow may call
them, but the tool definition itself is not a set of instructions to read.

### Central Agent

The central Agent is the single reasoning point (`internal/engine/`). It runs one
ReAct-style loop over a compiled `AgentContext`: each round it selects a read
capability, calls a knowledge tool, or proposes a write, and observes the typed
result in the same loop. There is no separate router and no per-request choice
between three execution forms.

### Read Capability

A read capability is a typed, model-visible read vertical
(`internal/capability/read_*.go`). Each owns its request struct, its **field
contract** (`field_contract.go`'s `schemaNode` — the single source for the tool
schema, runtime validation, and the consistency test), its handler, and its
renderer. `ReadDefinitions()` is the catalog; the engine dispatches through
`executeConcreteReadCapability` → `capability.MigratedRead(action)` →
`RegisteredRead.Run`. There is no route registry.

### Action Resolver

The action resolver (`internal/actionresolver/`) deterministically resolves the
target instance and spec of a proposed write (e.g. whether an image catalog must
be re-queried, via `SpecNeedsImageCatalog`). A write target is authorized only
when the user's reply deterministically resolves to it — the model does not
"interpret a candidate list" in place of deterministic target selection.

### Sealed Workflow

A sealed workflow (`internal/workflow/`) is a deterministic write process with
confirmation. It has a fixed step sequence, code-enforced safety checks, and no
free-form mutating tool calls by the model. Deployment, disk changes, lifecycle
ops (start/stop/reboot/reset-password/rename) and custom image creation are
sealed workflows. The `SealedActionContract` separates the confirmed action from
runtime metadata, and volatile fields (e.g. image) are re-confirmed before
execution.

Mutation stays behind workflow code and confirmation gates; a read capability or
`SearchKnowledge` may help explain or diagnose, but never mutates.

### Typed Observation

A tool result is not free text. A read capability returns a
`ReadCapabilityObservation` carrying status and a structured evidence envelope.
The Agent reasons over those facts and authors the final Markdown itself. The
ordinary read renderer's text is not a user-visible block and is never appended
to the Agent response a second time.

### Response Gateway

The response gateway (`engine`'s `finalizeResponse`) does not substitute or
append ordinary read results. It enforces the never-0% monitor invariant
(all-no-data historical monitor → whole-answer "cannot confirm", never
0%/healthy) and is the narrow server-side delivery path for credentials that
must not enter model context.

Note: ordinary read fidelity is intentionally delegated to the Agent, grounded
in the envelope facts; it is not a verbatim-insertion contract. The configured-
model monitor acceptance checks the reply against those facts without prescribing
the reply's wording or layout.

### RAG Evidence

RAG is retrieval used **inside** the Agent loop, via the `SearchKnowledge` tool —
not a terminal answer form. A multi-turn search first produces one standalone
answer question and one to three retrieval queries; a first-turn search skips that
extra planning call. Retrieval (`internal/knowledge/`, qwen3 RRF) returns cited
chunks the Agent grounds its answer in. Citation discipline is **fail-open**:
if the Agent cannot cite, the original answer ships with citation markers stripped
— citation formatting never regenerates user-facing prose. The only hard stop is
a raw-evidence leak (security). Citation-marker leakage into the final text is
caught by the output guard.

### Diagnosis Chain

A diagnosis chain (`internal/diagnosis/`) is a read-only diagnostic tool.
`DiagnoseBilling` is the only advertised one; `chainRegistry` equals the
advertised set, so an unadvertised diagnosis name cannot resolve (model-invisible
≠ unreachable). The SSH chain still exists and still runs, but not as a
`DiagnoseSSH` tool — `internal/capability/read_instance_access.go` constructs it
directly, so SSH reaches the model as `ReadCapability_instance_access`. That
precheck is explicitly cloud-side: it verifies the exact instance, lifecycle
state, structured login endpoint, and monitor risk signals, but does not probe a
public port or inspect the guest OS. Symptoms without a dedicated chain
(GPU/init/port/image) are handled by the central Agent gathering evidence via
`SearchKnowledge` + `DescribeCompShareInstance`.

### Memory

Memory is durable context preserving decisions, facts, and prior verification
results. It can guide future changes, but it is not a guardrail and must not
replace current-state verification.

### Guardrail

A guardrail is a code-enforced safety rule: destructive-action blocks, mutating
confirmation, tool policy checks, provenance gates, citation validation, input
interception, output redaction. Model-generated risk assessments are
observability signals; they must not become the source of truth for write safety.

### MCP Server / Client

MCP is a transport protocol — not a tool, capability, workflow, or RAG system. An
MCP *server* would expose selected read tools first (destructive actions never by
default, mutating workflow exposure requires confirmation support); an MCP
*client* consumes external MCP servers with allowlisting, namespacing, and
injection protection. Neither exists in the tree yet.

## Runtime Shape

```text
user request
  -> input guard (inputguard / guardrails)
  -> central Agent loop (engine): each round selects one of
       read capability  |  SearchKnowledge  |  propose write -> resolver -> sealed workflow
  -> response gateway (safety invariants / sensitive credential delivery)
  -> output guard (sanitizer / policy)
```

There is exactly one execution shape — the central Agent loop. Reads, knowledge
retrieval, and writes are choices *within* the loop, not separate top-level
pipelines.

## Naming Rules

- Use `central Agent` for the single engine loop; do not say "router" or "tier".
- Use `read capability` for a typed model-visible read vertical.
- Use `action resolver` for deterministic write-target/spec resolution.
- Use `sealed workflow` for deterministic mutating flows with confirmation.
- Use `typed observation` / `response gateway` for the result → render path.
- Use `RAG evidence` for retrieval consumed inside the loop (`SearchKnowledge`).
- Use `tool` for typed callable actions.
- Use `MCP server` / `MCP client` by direction; do not call MCP an agent layer.
- Do not use `routing`, `terminal_rag`, `true skill`, or `planned_execution_path`
  except when explicitly discussing the retired pre-P6 design.
