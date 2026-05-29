# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository

Go 1.22 CLI assistant ("优云算力共享 AI 助手") for the CompShare GPU platform. Single binary built from `cmd/`, with Python scripts under `scripts/rag_w0/` used only to build/eval the RAG corpus.

## Build & run

```bash
# Build the CLI
go build -o agent ./cmd                # Linux/macOS
go build -o agent.exe ./cmd            # Windows / cross-build via GOOS

# Run the CLI (reads deploy/conf/agent.yaml by default)
cp deploy/conf/agent.yaml.example deploy/conf/agent.yaml   # one-time, then fill ${ENV_VAR}s
./agent cli [-c path/to/agent.yaml]

go build -o agent ./cmd
./agent server --addr :8080
```

The config loader (`internal/config/config.go`) only supports plain `${ENV_VAR}` substitution — no `${VAR:-default}` syntax. Required env vars depend on the subcommand:

- `LLM_API_KEY` — required for all subcommands.
- `COMPSHARE_SERVICE_PUBLIC_KEY` / `COMPSHARE_SERVICE_PRIVATE_KEY` — service's own AK/SK used to call STS `AssumeRole`; optional for the `server` subcommand when legacy direct AK/SK is used instead.
- `COMPSHARE_DEFAULT_ROLE_URN` — required for the `cli` subcommand when STS mode is used.
- `MYSQL_DSN` — required for the `server` subcommand.
- `COMPSHARE_PUBLIC_KEY` / `COMPSHARE_PRIVATE_KEY` — legacy direct AK/SK; only needed when `agent.sts` is not configured (e.g., local dev without STS).

`project_id` may be left empty for read-only calls; HTTP requests can also pass `ProjectId` per request.

## Tests

```bash
go test ./... -count=1                       # full Go suite — required green before merge
go test ./internal/engine                    # one package
go test ./internal/engine -run TestName$     # one test
go test ./internal/entity -race -count=1     # entity package is race-checked in CI (.github/workflows/entity-race.yml)

# RAG corpus / scripts (Python; only when touching scripts/rag_w0/ or deploy/kb/)
python -m pytest scripts/test_rag_w0_scripts.py -q
```

The CLI golden suite is `eval/golden_test.go::TestGoldenScripts` (matches the 18 scripts in `eval/golden_scripts.md`); offline intent eval is `eval/evaluate_test.go::TestEval`. These are part of `go test ./...` — do not skip them.

## Pre-commit hook

`.githooks/pre-commit` runs `scripts/secret_scan.ps1` and **requires PowerShell** (`pwsh` or `powershell`) on PATH. If the hook is missing on a fresh clone:

```bash
git config core.hooksPath .githooks
```

## Runtime feature flags

Behavior is gated by env vars read in `cmd/trace.go` and `cmd/agent.go`. The default answer path uses the current demo stack: ds-v4-flash, qwen3 RRF retrieval, and LLM grounded rendering.

| Var | Values | Effect |
|---|---|---|
| `COMPSHARE_ENABLE_MUTATING_TOOLS` | `0` / `false` to disable | Gates start/stop/reboot/reset-password/create. **Default ON** (2026-05-29 cutover after PR2.5 production validation). Set `"0"` or `"false"` to force read-only mode. |
| `USE_INTENT_PLANNER` | `shadow` | Runs the LLM planner alongside ReAct for trace-only comparison. |
| `USE_INTENT_PLANNER_FOR` | default `resource,monitor,gpu_specs,stock,pricing,platform_image,custom_image,community_image`; explicit comma list overrides; `off` disables | Enables Phase-1 cutover: engine owns the planner call for those intents. |
| `USE_KNOWLEDGE_RETRIEVAL` | `curated` (default), `off` | Wires the RAG retriever into the engine. Combine with `RAG_RETRIEVAL_MODE`. |
| `RAG_RETRIEVAL_MODE` | `qwen3_rrf` (default), `bm25_only`, `hybrid_cosine`, `hybrid_rerank`, `qwen3_full` | Picks the retrieval pipeline. Hybrid/qwen3 modes require `MODELVERSE_API_KEY` or `LLM_API_KEY` and the matching pinned sidecar under `deploy/kb/`. |
| `RAG_HYBRID_ENABLED` | `1` | Legacy switch; only consulted when `RAG_RETRIEVAL_MODE` is unset. |
| `USE_GROUNDED_RENDERER` | `llm` (default), `off` | Routes final reply through `internal/renderer.GroundedRenderer`. |
| `COMPSHARE_TRACE_ENABLED` | `0` / `false` to disable | Writes per-turn JSONL traces to `COMPSHARE_TRACE_DIR`. **Default ON** (2026-05-29 cutover). Set `"0"` or `"false"` to force off for read-only test runs. |
| `MYSQL_DSN` | DSN string | Required by `compshare-agent server`; ignored by `compshare-agent cli`. |
| `COMPSHARE_SERVICE_PUBLIC_KEY` | AK string | Service long-term public key for STS `AssumeRole`. Required when `agent.sts` is configured. |
| `COMPSHARE_SERVICE_PRIVATE_KEY` | SK string | Service long-term private key for STS `AssumeRole`. Required when `agent.sts` is configured. |
| `COMPSHARE_DEFAULT_ROLE_URN` | URN string | Default role URN used by `cli` subcommand in STS mode. Overrides per-request `role_urn_template` derivation. |

Unknown values for any of the above are logged as warnings and treated as off — do **not** silently coerce them.

## Knowledge base — pinned digests

`deploy/kb/` holds the customer-safe FAQ corpus and embedding sidecars. All three artifacts are byte-pinned by LF-normalized SHA256 in `internal/knowledge/corpus_digest.go`:

- `stage2b_w0.jsonl` → `CorpusDigestExpected`
- `embeddings_<digest>.jsonl` (text-embedding-3-large, 3072d) → `EmbeddingDigestExpected`
- `embeddings_<digest>_qwen3-embedding-8b.jsonl` (qwen3, 4096d) → `EmbeddingDigestExpectedQwen3`

The loader **refuses to start** if any pin mismatches. When the corpus changes, regenerate **both** sidecars and update **all three** digest constants in the same change. See `deploy/kb/README.md` for the rebuild commands and PR #113/#114 for the 8-step flow.

## Architecture

### Entry path
`cmd/agent.go` (CLI loop) → `engine.Engine.Init()` → per-turn `Engine.Chat()`. `cmd/trace.go` is the env-flag wiring layer that builds the planner, retriever, renderer, and JSONL trace writer before injecting them into the engine.

### Engine (`internal/engine/`)
Runs a ReAct loop (`maxReActRounds=10`, `maxHistoryMessages=40`) with a tool-call budget per turn (`maxReadExpensiveCallsPerTurn=20`). Two dispatch paths coexist:
1. **Phase-1 cutover** — the default cutover set handles resource, monitor, GPU specs, stock, and image-list intents; `USE_INTENT_PLANNER_FOR` can override the set or disable it with `off`. `tryPhase1Cutover` calls handlers in `internal/intent/handler*.go` directly and emits `StepEvent`s without going through ReAct.
2. **ReAct** — default; the LLM picks tools registered in `internal/tools/registry.go`. Mutating tools are blocked unless `COMPSHARE_ENABLE_MUTATING_TOOLS=1`.

Force-tool / hard-block priority chain (highest first) is documented inline in `engine.go` and **must be kept in sync** when adding new force paths: account-billing-unsupported canned reply > monitor-recall force tool. Capability gating is required for any new object-`tool_choice` path: `ds-v4-flash` thinking mode 400s on object tool_choice, so callers must short-circuit when `supportsObjectToolChoice=false`.

### Intent / capability registry (`internal/intent/`)
Adding a capability is **data-only**. Append to `capabilityRegistry` in `capability_registry.go` and add a frontmatter markdown under `internal/intent/capabilities/*.md` (planner directives + examples live in the frontmatter and are embedded via `embed.FS`). Engine has a single generic `IsCapabilityIntent` / `DispatchCapability` hook — do **not** add per-case wiring there. `*.md.disabled` files are intentionally excluded.

### Workflow engine (`internal/workflow/`)
Multi-step mutating flows (create/start/stop/reboot/reset-password/rename) live as `*Workflow` types. Confirmation is delivered via the `engine.ConfirmFunc` callback (CLI implementation in `cmd/agent.go::cliConfirm`).

### Knowledge / RAG (`internal/knowledge/`)
Retriever modes are listed above. The RAG **system prompt** is composed from shared text snippets in `internal/prompt/rag_system_segments/` (ordered by `order.txt`), and the same snippets are read by the Python eval harness — keep both consumers in mind when editing. Reranker / embedder timeouts are knobbed by `RAG_HYBRID_TIMEOUT_MS` / `RAG_RERANKER_TIMEOUT_MS`.

### Diagnosis (`internal/diagnosis/`)
Read-only diagnostic tools (init failure, billing anomaly, GPU not detected, image issue, port/firewall, SSH failure). Boundary rule baked into prompts: read-only self-check commands may be suggested as user actions; commands that change environment must be marked as **optional fixes**, never auto-executed. Source-of-truth notes:
- SSH facts come from `DescribeCompShareInstance.SshLoginCommand`, **not** `DescribeCompShareSoftwarePort` (the latter currently returns image app ports, not SSH).
- Missing CPU/memory/GPU monitoring data must surface as "无法确认", never as 0%/healthy.

### Observability (`internal/observability/`)
`observability.Writer` writes one JSONL line per turn. `cliTraceRecorder` in `cmd/trace.go` is the bridge that wires planner/retrieval/renderer/token-usage observers into the writer. Retention: `DefaultTraceRetentionDays`, cleaned on each run.

### Other notable boundaries
- `internal/security/secret_boundary.go` + `internal/sanitizer/` — keep redaction logic centralized; do not inline new redaction in tools.
- `internal/policy/leakage.go` — citation-leakage guards used by the cited-strip pass in the engine.
- `internal/governance/ratelimit.go` — QPS/daily limits live in `agent.rate_limit` config and are enforced for LLM, mutating, and read-expensive call classes.
- `internal/entity/` — only Go package run with `-race` in CI; concurrent registry access is a known concern there.

## HTTP service

`compshare-agent server` runs the HTTP gateway alongside the CLI; both share the engine/knowledge/planner core.

- Entry: `cmd/server.go`. Routes: `POST /` (Action-routed) + `GET /healthz`.
- Identity is taken from the request body (gateway-injected), not headers: `top_organization_id` / `organization_id` (uint32, snake_case) and `request_uuid` (string, snake_case, auto-generated if missing). Business fields stay PascalCase (`Action`, `SessionId`, `Message`).
- Phase-1 Actions: `GetSession` / `CreateSession` / `Chat` (SSE) / `GetMeta` / `Feedback`. `SessionId` is mandatory on every session-scoped Action; the frontend persists it in localStorage.
- Per-session `*engine.Engine` lives in `internal/agentpool` (LRU 200 / 30min idle). HTTP path skips `engine.Init()` and rehydrates history from MySQL via `engine.RehydrateHistory`.
- SSE stream is per-token end-to-end via `llm.ChatRequest.OnTextDelta` → `engine.ChatOptions.OnTextDelta` → `sse.Writer`. ReAct intermediate `StepEvent`s are not exposed in phase 1.
- Persistence: MySQL 8 via `database/sql + go-sql-driver/mysql`; schema in `deploy/migrations/0001_init.sql`. `messages` is INSERTed twice per turn (user immediately, assistant placeholder before LLM call) and UPDATEd once on SSE done — never per-token. DDL is run by ops, not the binary.
- Credentials: HTTP path prefers STS AssumeRole when `agent.sts.service_ak/service_sk` are set. If they are empty, it falls back to legacy `agent.public_key/private_key` for local/demo use. Rate limiting is keyed by `(top_organization_id, organization_id)` pair, not by static public key.

## Conventions specific to this repo

- The runtime ships with **mutating tools enabled by default** (2026-05-29 cutover after PR2.5 production validation). Use `COMPSHARE_ENABLE_MUTATING_TOOLS="0"` for read-only test runs or emergency disable; never delete the override path.
- Static FAQ text was removed from the ReAct prompt — platform knowledge flows only through the RAG retriever. Do not reintroduce `FAQContent` / `ReadOnlyFAQContent` injection (`internal/prompt/builder_test.go` has reverse assertions).
- Shadow QA per-round configs under `eval/shadow_qa/**/agent.yaml` and `.env` files are git-ignored and contain real keys — never commit anything matching those globs.
- When adding planner examples, group by intent and record a one-line source for each example; tests in `internal/intent/planner_prompt_test.go` enforce grouping/tool/intercept consistency.
- `SecurityToken` must be included in API signing params before computing the HMAC-SHA1 signature. See `internal/tools/README.md` §6 for the six common pitfalls.
