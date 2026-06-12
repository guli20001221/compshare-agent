# Agent-Skill Runtime Architecture

This document defines the runtime terms used by the Youyun CompShare console
agent. Its purpose is to keep routing workflows, terminal retrieval, true
skills, tools, saga workflows, MCP, and guardrails separate in both code and
review language.

## Terms

### Tool

A tool is a typed callable action. It has a name, input schema, policy level,
and execution result. Console API wrappers such as instance description,
pricing, stock, image listing, and mutating API calls are tools.

Tools are not playbooks. The model or a deterministic workflow may call them,
but the tool definition itself is not a set of instructions to read.

### Routing

`routing` is deterministic classify-then-dispatch for stable read-only console
requests. It chooses a known route and runs a fixed handler over typed tools.

Routing entries may be authored from metadata, but they are not true skills at
runtime because the model does not read their body and does not select them by
progressive disclosure.

### Terminal RAG

`terminal_rag` is a retrieval workflow for pure knowledge answers. It retrieves
knowledge, validates citation requirements, and returns a final answer.

Terminal retrieval is route-protected. It is not the same thing as using
retrieval as an internal evidence source inside a longer agent process.

### True Skill

A true skill is a model-read playbook. It is selected by name and description,
then its body is loaded into model context, with deeper files disclosed only
when needed.

True skills belong to the `agent` runtime form. They are appropriate for
open-ended diagnosis and other read-only reasoning processes where a fixed
workflow would be too brittle.

### Saga Workflow

A saga workflow is a deterministic write process with confirmation. It has a
fixed sequence of steps, code-enforced safety checks, and no free-form mutating
tool calls by the model.

Write operations such as deployment, disk changes, and custom image creation
belong in saga workflows, not true skills. A skill may help explain or diagnose,
but mutation stays behind workflow code and confirmation gates.

### RAG Evidence

RAG evidence is retrieval used inside another process, such as diagnosis. It is
not a terminal answer by itself.

Diagnosis may use retrieval as evidence only after a citation-aware adapter
exists. Until then, diagnosis retrieval probes must stay observability-only and
must not inject raw chunk text into a skill prompt or final answer.

### Memory

Memory is durable context used to preserve decisions, facts, and prior
verification results. It can guide future changes, but it is not a guardrail and
must not replace current-state verification.

### Guardrail

A guardrail is a code-enforced safety rule. Examples include destructive-action
blocks, mutating-action confirmation, tool policy checks, provenance gates, and
citation validation.

Planner labels and model-generated risk assessments are observability signals.
They must not become the source of truth for write safety.

### MCP Server

An MCP server exposes selected tools, resources, or prompts to an external host.
For this project, server exposure should start with read tools. Destructive
actions are never exposed by default, and mutating workflow exposure requires
confirmation support.

### MCP Client

An MCP client consumes external MCP servers. External tools must be allowlisted,
namespaced, and protected against prompt/resource injection.

MCP is a transport protocol. It is not a tool, a skill, a workflow, or a RAG
system.

## Runtime Forms

The runtime has three observable execution forms:

```text
user request
  -> planner predicts planned_execution_path
  -> runtime executes one of:
       routing
       terminal_rag
       agent
```

### routing

`routing` handles stable read-only console questions through deterministic
handlers. It is the right form for common catalog, status, pricing, and
availability queries when the behavior is already well-defined.

### terminal_rag

`terminal_rag` handles pure knowledge requests where the correct response is a
cited answer from the knowledge base.

### agent

`agent` contains both true skills and saga workflows:

- true skills: body-read, model-driven playbooks for open-ended read-only work.
- saga workflows: deterministic write flows with confirmation and guardrails.

These are both in the agent form because they are outside the deterministic
read-only route. They are still different runtime primitives and must not be
merged.

## Current Repo State

- Deterministic read-only routing already exists for several catalog and status
  requests.
- `knowledge_qa` already implements terminal retrieval and must keep citation
  protection.
- Deployment is a saga workflow arm, not a true body-read skill.
- Diagnosis body-read execution exists only behind explicit rollout gates and a
  diagnosis allowlist.
- Disk creation already has a workflow.
- Custom image creation is a high-value mutating action, but it still needs a
  saga workflow with confirmation and progress follow-up.
- Deterministic routing entries now live in route manifests
  (`internal/routing/<name>/route.yaml`); the skill-shaped files
  (`internal/skills/<name>/SKILL.md`) hold only the agent-lane diagnosis
  skills. The earlier skill-shaped routing entries were an authoring artifact,
  since migrated — they were never the runtime definition of a true skill.

## Naming Rules

- Use `routing` for deterministic read-only dispatch.
- Use `terminal_rag` for cited final knowledge answers.
- Use `agent` for true skills and saga workflows.
- Use `true skill` only when the model reads the skill body.
- Use `saga workflow` for deterministic mutating flows.
- Use `tool` for typed callable actions.
- Use `RAG evidence` for retrieval consumed inside another process.
- Use `MCP server` and `MCP client` by direction.
- Do not call deterministic routing entries true skills at runtime.
- Do not call saga workflows true skills.
- Do not call MCP an agent layer.

## Migration Sequence

1. Add observe-only `planned_execution_path` trace derived from existing intent.
2. Keep this terminology document as the naming source for future reviews.
3. Add high-value saga workflows one at a time, starting with custom image
   creation.
4. Move deterministic routing entries out of skill-shaped authoring files into
   route manifests.
5. Stabilize the body-read diagnosis lane with process evals and strict
   read-only tool visibility.
6. Add a citation-aware RAG evidence adapter before any diagnosis skill consumes
   retrieval content.
7. Add actual `execution_path` trace after trace sources can distinguish what
   really executed.
8. Split MCP work into server and client directions only after the internal
   boundaries are stable.
9. Expand eval coverage to measure planned form accuracy, actual-form mismatch,
   true-skill selection, confirmation behavior, citation validation, and
   retrieval leakage.
