# Compshare Agent

This repository contains the Go service behind the 优云智算 assistant. The
production entrypoint is the HTTP/WebSocket server in `cmd/`; there is no CLI
chat runtime.

## Build and test

```bash
go build ./...
go vet ./...
go test ./... -count=1

# Run the server with the checked-in baseline configuration.
go run ./cmd server -c deploy/conf/config.local.yaml
```

Before committing, also run `git diff --check`. The pre-commit hook invokes
`scripts/secret_scan.ps1` and needs PowerShell on `PATH`:

```bash
git config core.hooksPath .githooks
```

Live-model tests and PostgreSQL integration tests are opt-in. Their test files
name the required flags and credentials; a normal `go test ./...` must not make
external calls.

## Runtime architecture

The semantic runtime has one path:

```text
HTTP/WS request
  -> session engine from internal/agentpool
  -> engine.ChatWithOptions
  -> internal/agentruntime ReAct loop
  -> model tool call / tool result / final answer
  -> response gateway
  -> assistant row + canonical transcript metadata
```

`internal/agentruntime` owns round progression and runtime events. `Engine`
assembles product tools, context and policy around that loop. Do not add a
second planner, semantic router, hidden answerer or alternate turn executor.

Canonical transcript replay is always enabled. Each completed turn persists a
bounded `agent_transcript_v1` containing the model-visible user, assistant,
tool-call and tool-result messages. A later turn replays that transcript; it
does not reconstruct semantic history from summaries.

The model-visible context card contains only current execution context such as
a selected instance or a pending candidate. It is not a second memory, and
semantic summaries or fact caches must not be injected beside the transcript.
The answer verifier keeps a model-invisible evidence ledger. Workflow forms,
confirmations, idempotency records and selected-instance provenance are
transaction/authorization state, not semantic memory, and remain deterministic.

History is bounded by size, not message or exchange counts:

- `maxReplayedHistoryRunes` budgets prior exchanges.
- `maxAssembledRequestRunes` charges messages plus the serialized tool window.
- assembly removes oldest history before oldest current-turn tool groups;
  system messages, the current question and tool-call/result pairing remain
  intact.

ReAct result projection is always enabled for supported large read results. It
changes only the model-visible copy; trace keeps the full structured result and
records whether projection occurred.

## Tool and workflow boundaries

The model chooses read capabilities and knowledge retrieval in the loop.
Production knowledge retrieval always uses the remote MCP configured at
`agent.retrieval.mcp_url`. The in-process retriever is for tests and offline
evaluation only.

There is no keyword topic router or lexical jailbreak/off-topic pre-block in
front of the Agent. Scope belongs in the system prompt. Natural-language support
requests also reach the central Agent, which may call `HandoffToCustomerSupport`;
the active channel renders the actual support entry, so the model never authors
QR codes or adapter markers. Only an explicit structured transport event may
bypass semantic interpretation.

Model-visible read capabilities live in `internal/capability/`. Each capability
owns its typed request, schema contract, handler and renderer. Do not recreate a
parallel route registry.

Mutating operations live in `internal/workflow/` and are proposed through the
central Agent. They remain subject to all of these controls:

1. the deployment authorization `agent.authorization.mutating_tools`;
2. exact target resolution and selected-instance provenance;
3. permission and policy checks;
4. a user confirmation card;
5. workflow idempotency and write-side revalidation.

Destructive/L2 actions stay refused regardless of the mutating-tools setting.
Do not turn these controls into prompt instructions or let the model attest its
own authorization.

Editable confirmation forms and guided creation are stable protocol features.
The server advertises `confirm_form_v1` and `guided_create_v1`; a client must opt
in on the turn before it receives those shapes. They are not server rollout
flags.

Model-owned read-tool arguments rejected by schema, grounding, or live-catalog
validation use
`status=needs_input,next_step=correct_tool_call` only with
`INVALID_TOOL_ARGUMENTS`. They must not be converted into a question to the
user. Incomplete provider output (`finish_reason` truncation or partial tool
calls) is never persisted or executed as a normal answer.

## In-instance diagnosis

`DiagnoseInstanceInternals` is an optional SSH-ops lane. It requires:

- tenant-scoped STS credentials;
- a configured Python/Agent-SDK harness;
- audit migrations `0011`, `0013` and `0014`;
- explicit entry confirmation and, when writes are enabled, per-command write
  confirmation.

The lane fails closed when audit storage is unavailable. A missing audit schema
disables only this lane and logs the missing migration; it does not take chat
down. Destructive effects, command substitution and guest-shell reboot remain
refused. Multiline commands, pipes and chains are classified by effect, and
guest changes still require exact confirmation. The definitive command policy
and deployment contract live in `deploy/ssh_ops_harness/README.md` and its
tests—do not duplicate their full history in config comments.

Production address routing is configured in `config.prod.yaml`: UHost internal
mapping first, the translated public-IPv4 candidate second. The advertised public
EIP is diagnostic-only and is never selected as a dial target.

If a browser disconnects during a diagnosis, the current process may show a
bounded deterministic notice on the next turn. It does not resume or replay
commands, does not enter model history, and does not survive process/LRU loss.

## Configuration

`deploy/conf/config.local.yaml` is the shared deployment baseline.
`config.prod.yaml` extends it with production-network overrides. YAML is the
source of truth; environment variables remain compatibility fallbacks for the
small number of operational controls implemented by `RuntimeGetenv`.

Only genuine operational or authorization choices remain configurable:

| Setting | Purpose |
|---|---|
| `agent.authorization.mutating_tools` | authorize confirmation-gated product writes |
| `agent.retrieval.mcp_*` | remote knowledge MCP endpoint/token/timeout |
| `agent.trace.*` | completed-turn trace sink and retention inputs |
| `agent.ssh_ops.*` | optional in-instance lane, permissions and network routing |
| `agent.ocr.*` | screenshot interpretation |
| `agent.feishu.*` | Feishu adapter and public-channel scope |
| `agent.rate_limit.*` | tenant request/token budgets |

Do not add a feature flag for code that has only one supported production
behavior. A rollback that requires restoring deleted semantics is a code revert,
not a boolean branch kept forever.

## HTTP and persistence

`cmd/server.go` exposes `POST /`, WebSocket chat, and `GET /healthz`. Gateway
identity comes from `top_organization_id`, `organization_id` and `request_uuid`;
business fields use the existing PascalCase API contract.

Per-session engines live in `internal/agentpool` (bounded LRU with idle expiry).
Persisted user/assistant rows rebuild a cold engine, and assistant metadata
reattaches the canonical tool transcript. PII/credential redaction must remain
centralized in `internal/security` and `internal/sanitizer`, with the same
role-specific representation on hot and cold paths.

The store is PostgreSQL via `database/sql` and `lib/pq`. Historical names such
as `mysql`, `MYSQL_DSN`, `OpenMySQL` and `MySQLMessageStore` remain API/config
compatibility names; do not infer the database type from them. Apply all SQL
migrations in lexical order before deployment. See
`deploy/migrations/README.md`.

SSE final answers are attempt-atomic and final-answer-atomic, not true upstream
token streaming: failed retries and incomplete output must never leak as a
prefix. Tool/confirmation activity uses separate step frames.

## Observability

`internal/httpapi/trace_recorder.go` writes one content-free trace per completed
turn through `internal/observability`. Trace may contain model/provider IDs,
finish reasons, token counts, tool actions/error classes/latencies, prompt
section IDs, request-size peaks, selected-instance provenance and workflow
outcomes. It must not become a second conversation database: no raw prompt,
reply, tool payload, credential or canonical transcript.

When adding a tool failure, use the existing closed error-code/error-class
contracts rather than parsing error messages. Preserve three-state metrics where
absence differs from zero.

## Repository conventions

- The central system prompt is assembled from `internal/prompt/segments.go`.
  Follow `docs/dev/prompt-change-checklist.md` for prompt changes.
- Platform facts come from tools or RAG. Do not add static FAQ text or model-name
  mappings to the system prompt.
- `internal/entity` is race-tested in CI; keep registry access synchronized.
- `SecurityToken` participates in API signing parameters before HMAC-SHA1. See
  `internal/tools/README.md`.
- Preserve unrelated dirty-worktree changes. Stage explicit files, never
  `git add -A` in a shared workspace.
