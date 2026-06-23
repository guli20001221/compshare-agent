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
- `MYSQL_DSN` — required for the `server` subcommand. PostgreSQL libpq URL (`postgresql://user:pass@host:5432/db?sslmode=disable`); the env var name is kept for compat.
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

**Preferred config is YAML (`deploy/conf/agent.yaml`).** The flags below now have typed fields under `agent.features` / `agent.retrieval` / `agent.trace` / `agent.planner` (see `internal/config/runtime.go` + `agent.yaml.example`). Precedence is **YAML wins, env is the fallback**: a field set in YAML overrides the env var; a field omitted in YAML falls through to the env var, then to the built-in default. The bridge is `(*config.Config).RuntimeGetenv`, which overlays the YAML fields on `os.Getenv` so the `cmd/` parsers still read every flag through one `getenv` (wired in `cmd/server.go` + `cmd/cli.go`). Secrets may also be inlined in YAML now (loader accepts literals; the committed `agent.yaml.example` keeps `${ENV_VAR}` placeholders and `deploy/conf/agent.yaml` is gitignored). The env-var table below stays valid as the fallback / per-flag reference. The default answer path uses the current demo stack: ds-v4-flash, qwen3 RRF retrieval, and LLM grounded rendering.

| Var | Values | Effect |
|---|---|---|
| `COMPSHARE_ENABLE_MUTATING_TOOLS` | `1` | Enables start/stop/reboot/reset-password/create. Default off — read-only mode. |
| `COMPSHARE_INTENT_ROUTER_MODE` | `shadow` | Runs the LLM intent router alongside ReAct for trace-only comparison. |
| `COMPSHARE_DIRECT_DISPATCH_INTENTS` | default `resource,monitor,gpu_specs,stock,pricing,platform_image,custom_image,community_image`; explicit comma list overrides; `off` disables | Enables direct dispatch: the engine owns the intent-router call for those intents and dispatches deterministically (no ReAct). |
| `USE_KNOWLEDGE_RETRIEVAL` | `curated` (default), `off` | Wires the RAG retriever into the engine. Combine with `RAG_RETRIEVAL_MODE`. |
| `RAG_RETRIEVAL_MODE` | `qwen3_rrf` (default), `bm25_only`, `hybrid_cosine`, `hybrid_rerank`, `qwen3_full` | Picks the retrieval pipeline. Hybrid/qwen3 modes require `MODELVERSE_API_KEY` or `LLM_API_KEY` and the matching pinned sidecar under `deploy/kb/`. |
| `RAG_HYBRID_ENABLED` | `1` | Legacy switch; only consulted when `RAG_RETRIEVAL_MODE` is unset. |
| `USE_GROUNDED_RENDERER` | `llm` (default), `off` | Routes final reply through `internal/renderer.GroundedRenderer`. |
| `COMPSHARE_EXTERNAL_KNOWLEDGE` | default **on**; `0`/`off`/`false` disables | Merges the external tool/ops corpus (`deploy/kb/external_w0.jsonl`: vLLM/SGLang/Ollama/ComfyUI/GPU + Linux-ops/env-mgmt/PyTorch-basics + model-download) into the qwen3 retrieval index. Additive; `loadKnowledgeCorpora` degrades to platform-only if the external file is missing/bad. Boot-only; set `0` to roll back to platform-only. |
| `COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE` | default **on**; `0`/`off`/`false` disables | Exposes the read-only `SearchKnowledge` registry tool so a symptom/tool-ops turn retrieves prior tool/ops evidence before `Diagnose*`. Boot-only (resolved in `cmd`, frozen via `tools.SetAgenticSearchKnowledgeEnabled`; Go-package default stays off so unit tests are unaffected); set `0` to roll back. Enabled together with `COMPSHARE_EXTERNAL_KNOWLEDGE`. |
| `COMPSHARE_RAG_GROUNDED_VALIDATOR` | `1` | Enables the route-independent grounded-answer (cite + leak) validator on the agentic `SearchKnowledge` synthesis (#126). When on, the tool result carries a `[[chunk_id]]` cite protocol and a synthesis that cites no retrieved chunk (or cites an unknown one) is replaced with the canned no-evidence reply (the raw-leak guard still runs first). **Scope:** the cite-or-refuse contract applies only when SearchKnowledge surfaced evidence the agent was shown; a weak/empty-evidence turn (relevance floor dropped all hits → empty ledger) is **not** gated — there is nothing to cite, so the answer falls back to the un-gated agent reply. **Default off** — boot-only, frozen via `engine.SetGroundedAnswerValidatorEnabled`; the Go-package default stays off. Deliberately separate from `COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE`: kept off until a flag-on eval proves the agent cites at the 100%-cite / 0-leak bar; flipping it on is a separate eval-gated PR. |
| `COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP` | default **on** (2026-06-09); `0`/`off`/`false` disables | Routes a `knowledge_qa` turn into the shared ReAct loop with a **forced `SearchKnowledge` first hop** instead of the deterministic terminal-RAG route (`tryStage2BRetrieval`) — the lead's "no separate RAG; RAG is a tool the agent calls in-loop" north star. The agent keeps the option to call `SearchKnowledge` again on later rounds (multi-hop preserved); the final answer is written by the disciplined-synthesis primitive (`COMPSHARE_KNOWLEDGE_QA_DISCIPLINED_SYNTHESIS`, also default-on). Inert unless `COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE` is also on (the tool must be visible — the engine route gate enforces this so the forced hop never names an absent tool) and a retriever is wired. Preserves cite-or-refuse turn-scoped and reports a distinct `dispatched_knowledge_agent_loop` route status with planned runtime-form `agent` (so the runtime-form mismatch gate doesn't false-flag). Boot-only, frozen via `engine.SetKnowledgeQAAgentLoopEnabled`; Go-package default stays off so unit tests are unaffected. Flip gated on the #150 A/B: the decisive code-heavy probe (PyTorch DDP, N=20) matched terminal RAG at refusal 0.00 / 0 fabrication / 0 contamination (opus-4-7 judge); the other 7 real-tone probes were already 0-refusal. The terminal route (`tryStage2BRetrieval`) is retained as the `=0` rollback, not deleted. |
| `COMPSHARE_KNOWLEDGE_QA_DISCIPLINED_SYNTHESIS` | default **on** (2026-06-09); `0`/`off`/`false` disables | Effective only when `COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP` is also on: the final knowledge_qa answer is written by terminal RAG's tight cited-synthesis prompt (`answerWithRetrievedEvidence`, with its own cite-harder retry) on the evidence the agent gathered via `SearchKnowledge` — instead of the free ReAct write, which under flash intermittently omits the cite or dumps raw text. This is what made the agent loop match terminal on faithfulness/refusal (DDP N=20: refusal 0.00, 0 fab). On synthesis failure it falls through to the existing cite-retry/refusal, so it is never worse than free-write. Boot-only, frozen via `engine.SetDisciplinedKQASynthesisEnabled`; Go-package default stays off so unit tests are unaffected. Set `0` to roll back to the free ReAct write + cite-retry. |
| `COMPSHARE_CONFIRM_FORM` | `1` | **Server-only.** Boot half of the editable-confirm-form double gate (create-flow 表单化, `docs/plans/2026-06-10-create-flow-form-confirm.md`): with it on AND the client opting in per turn (`SendCSAgentChat` `Features:["confirm_form_v1"]`), `confirmation` frames for `CreateInstanceWorkflow` carry a select-only `Form` (GPU/zone/image/charge-type whitelists) and `ConfirmCSAgentAction` may return `Overrides`; every edit re-runs the stock+price steps and re-confirms a refreshed card (≤3 edits). Default off — confirmation frames stay byte-identical, Overrides rejected. CLI confirm and the deploy_model saga are unaffected either way. |
| `COMPSHARE_TRACE_ENABLED` | `1` | Writes per-turn JSONL traces to `COMPSHARE_TRACE_DIR`. |
| `USE_SESSION_FACT_CONTEXT` | `1` | Injects a near-term fact cache (recent instance state, ~5min TTL) into context. Server-only wiring. **Go code default off; deploy template ships it on** (`.env.example`=1 + `invite.sh` forwards). |
| `USE_REACT_RESULT_PROJECTION` | `1` | Compresses large read tool results (list endpoints) before re-feeding ReAct. **Go code default off; deploy template ships it on.** |
| `USE_REACT_HISTORY_COMPACTION` | `1` | Summarizes old turns once history exceeds the window. **Go code default off; deploy template ships it on.** |
| `COMPSHARE_INTENT_ROUTER_STRUCTURED_OUTPUT` | `json_object` \| `json_schema` | Forces the intent router to emit via `response_format`. `json_object` requests bare JSON; `json_schema` (2026-06-23) requests the typed `IntentRoute` schema (`intent.IntentRouteResponseSchema`, **non-strict** — ds-v4-flash enforces the enum/const even without `strict`; live-probed). `json_schema` only takes effect when the model capability resolves to `OutputModeJSONSchema` (`SupportsJSONSchema`), degrading to `json_object` on object-only models. **Plumbed through `invite.sh` but shipped OFF** — the earlier `json_object` A/B showed no schema-valid improvement (json_object carries no schema); enable `json_schema` only after the intent-router accuracy A/B validates it. |
| `MYSQL_DSN` | DSN string | PostgreSQL libpq URL (env var name kept for compat). Required by `compshare-agent server`; ignored by `compshare-agent cli`. |
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
1. **Direct dispatch** (formerly "Phase-1 cutover") — the default direct-dispatch set handles resource, monitor, GPU specs, stock, and image-list intents; `COMPSHARE_DIRECT_DISPATCH_INTENTS` can override the set or disable it with `off`. `tryRouteDispatch` calls handlers in `internal/intent/handler*.go` directly and emits `StepEvent`s without going through ReAct.
2. **ReAct** — default; the LLM picks tools registered in `internal/tools/registry.go`. Mutating tools are blocked unless `COMPSHARE_ENABLE_MUTATING_TOOLS=1`.

Force-tool / hard-block priority chain (highest first) is documented inline in `engine.go` and **must be kept in sync** when adding new force paths: unsupported-historical-monitor canned reply > monitor-recall force tool (the account-billing-unsupported keyword hard-block was removed 2026-06-10 — that intent now dispatches to ReAct). Capability gating is required for any new object-`tool_choice` path: callers must short-circuit when `supportsObjectToolChoice=false` — gate on the per-model `llm.Capability` flag, never a hardcoded model name (e.g. `ds-v4-flash` 400'd on object tool_choice in the 2026-05-08 probe but was re-probed `true` on 2026-06-08, so the flag now reads true for it).

### Intent / route registry (`internal/routing/`, `internal/skills/`, `internal/intent/`)
Adding a route is **data-only**. Author `internal/routing/<name>/route.yaml` (frontmatter: `intent_label`, `handler_key`, `required_tools`/`tool_subset`, `planner_directives` + `planner_examples`, `provenance`) and regenerate with `go generate ./internal/routing/` (`cmd/routegen` → `registry_gen.go`). That generated registry, surfaced via `routing.GeneratedRoutes()`, is the **sole** route-dispatch + planner-prompt source. Bind the route's `handler_key` string to its Go handler in `internal/intent/skill_registry.go` (`RouteHandlerForKey`). Engine dispatches through a single generic `intent.IsRoutingIntent` / `handler.DispatchRoute` hook — do **not** add per-case wiring there. Diagnose skills follow the same generate-from-frontmatter pattern under `internal/skills/<name>/SKILL.md` (`cmd/skillgen` → `go generate ./internal/skills/`). The legacy `capabilityRegistry` / `capability_registry.go` / `internal/intent/capabilities/*.md` / `IsCapabilityIntent` / `DispatchCapability` were retired in #115 — the term **capability** now refers only to model capability (`llm.Capability`, e.g. `supportsObjectToolChoice`).

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
- `internal/ocr/` (screenshot understanding, server/WS-only) — when `SendCSAgentChat` carries an `Image`, a Qwen3-VL call (`agent.ocr.model`, e.g. `qwen3-vl-flash`; empty = disabled) interprets the screenshot to **structured text** that is injected as context (it is NOT plain OCR, and the raw image never reaches the main model). The vision prompt is `ocr.DefaultPrompt`, overridable via `agent.ocr.prompt` (empty/whitespace = default, never an empty instruction). Trust boundary: recognized screenshot text is **untrusted reference context** — fenced via `engine.WrapScreenshotContext` (the single producer for both the live turn and the persisted/rehydrated copy), interprets-but-does-not-prescribe-fixes, runs through `RedactPII`, and feeds only conversation history (never routing/force-tool/hard-block, which use the raw user message). It must never auto-drive a mutating action; the confirmation gate / `COMPSHARE_ENABLE_MUTATING_TOOLS` remains the hard stop.

## HTTP service

`compshare-agent server` runs the HTTP gateway alongside the CLI; both share the engine/knowledge/planner core.

- Entry: `cmd/server.go`. Routes: `POST /` (Action-routed) + `GET /healthz`.
- Identity is taken from the request body (gateway-injected), not headers: `top_organization_id` / `organization_id` (uint32, snake_case) and `request_uuid` (string, snake_case, auto-generated if missing). Business fields stay PascalCase (`Action`, `SessionId`, `Message`).
- Phase-1 Actions: `GetSession` / `CreateSession` / `Chat` (SSE) / `GetMeta` / `Feedback`. `SessionId` is mandatory on every session-scoped Action; the frontend persists it in localStorage.
- Per-session `*engine.Engine` lives in `internal/agentpool` (LRU 200 / 30min idle). HTTP path skips `engine.Init()` and rehydrates history from PostgreSQL via `engine.RehydrateHistory`.
- SSE stream is per-token end-to-end via `llm.ChatRequest.OnTextDelta` → `engine.ChatOptions.OnTextDelta` → `sse.Writer`. ReAct intermediate `StepEvent`s are not exposed in phase 1.
- Persistence: PostgreSQL via `database/sql + lib/pq` (migrated from MySQL/TiDB; the `store.OpenMySQL` symbol, `internal/store/mysql.go` file, `mysql` config key, and `MYSQL_DSN` env var name are all kept for compat but open a `postgres` connection). Schema in `deploy/migrations/0001_init.sql` (PG-dialect; apply with `psql`). `messages` is INSERTed twice per turn (user immediately, assistant placeholder before LLM call) and UPDATEd once on SSE done — never per-token. DDL is run by ops, not the binary.
- Credentials: HTTP path prefers STS AssumeRole when `agent.sts.service_ak/service_sk` are set. If they are empty, it falls back to legacy `agent.public_key/private_key` for local/demo use. Rate limiting is keyed by `(top_organization_id, organization_id)` pair, not by static public key.

## Conventions specific to this repo

- The runtime is **read-only by default in Go code** (the binary refuses mutating tools unless `COMPSHARE_ENABLE_MUTATING_TOOLS=1`). By deliberate decision the **production deploy template ships it on**: `.env.example` sets `COMPSHARE_ENABLE_MUTATING_TOOLS=1` and `deploy/scripts/invite.sh` forwards it, so a packed/deployed console enables write ops out of the box. Destructive / L2 actions (delete, terminate) stay refused regardless (`internal/tools/safe_executor.go`). Never set the flag in **tests** (mutating tests use the workflow registry directly), and keep the **Go code default off** — only the deploy template enables it.
- Static FAQ text was removed from the ReAct prompt — platform knowledge flows only through the RAG retriever. Do not reintroduce `FAQContent` / `ReadOnlyFAQContent` injection (`internal/prompt/builder_test.go` has reverse assertions).
- Shadow QA per-round configs under `eval/shadow_qa/**/agent.yaml` and `.env` files are git-ignored and contain real keys — never commit anything matching those globs.
- When adding planner examples, group by intent and record a one-line source for each example; tests in `internal/intent/planner_prompt_test.go` enforce grouping/tool/intercept consistency.
- `SecurityToken` must be included in API signing params before computing the HMAC-SHA1 signature. See `internal/tools/README.md` §6 for the six common pitfalls.
