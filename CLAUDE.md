# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository

Go 1.22 CLI assistant ("优云算力共享 AI 助手") for the CompShare GPU platform. Single binary built from `cmd/`, with Python scripts under `scripts/rag_w0/` used only to build/eval the RAG corpus.

## Build & run

```bash
# Build the CLI
go build -o agent ./cmd                # Linux/macOS
go build -o agent.exe ./cmd            # Windows / cross-build via GOOS

# Run the CLI (reads deploy/conf/config.yaml by default)
./agent cli [-c path/to/config.yaml]

go build -o agent ./cmd
./agent server --addr 0.0.0.0:7429
```

The deploy config is `deploy/conf/config.yaml`. Runtime flags, model keys,
CompShare credentials, and PostgreSQL DSN are written directly in that file.
Do not add new `.env` / `*.example` deployment flows.

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

**Config is YAML (`deploy/conf/config.yaml`).** Runtime flags have typed fields under `agent.features` / `agent.retrieval` / `agent.trace` / `agent.planner` (see `internal/config/runtime.go`). The deploy file also carries secrets directly: LLM key, STS service AK/SK, role URN, and PostgreSQL DSN. The env-var names below are still the historical parser names in code, but deployment must set the matching YAML fields instead of exporting environment variables. The default answer path uses the current demo stack: ds-v4-flash, qwen3 RRF retrieval, and LLM grounded rendering.

| Var | Values | Effect |
|---|---|---|
| `COMPSHARE_ENABLE_MUTATING_TOOLS` | `1` | Enables start/stop/reboot/reset-password/create. Default off — read-only mode. |
| `COMPSHARE_INTENT_ROUTER_MODE` | `shadow` | Runs the LLM intent router alongside ReAct for trace-only comparison. |
| `COMPSHARE_DIRECT_DISPATCH_INTENTS` | default `resource,monitor,gpu_specs,stock,pricing,platform_image,custom_image,community_image`; explicit comma list overrides; `off` disables | Enables direct dispatch: the engine owns the intent-router call for those intents and dispatches deterministically (no ReAct). |
| `USE_KNOWLEDGE_RETRIEVAL` | `curated` (default), `off` | Wires the RAG retriever into the engine. Combine with `RAG_RETRIEVAL_MODE`. |
| `RAG_RETRIEVAL_MODE` | `qwen3_rrf` (default), `bm25_only`, `hybrid_cosine`, `hybrid_rerank`, `qwen3_full` | Picks the retrieval pipeline. Hybrid/qwen3 modes require `MODELVERSE_API_KEY` or `LLM_API_KEY` and the matching pinned sidecar under `deploy/kb/`. |
| `RAG_HYBRID_ENABLED` | `1` | Legacy switch; only consulted when `RAG_RETRIEVAL_MODE` is unset. |
| `USE_GROUNDED_RENDERER` | `llm` (default), `off` | Routes final reply through `internal/renderer.GroundedRenderer`. |
| `COMPSHARE_EXTERNAL_KNOWLEDGE` | default **on**; `0`/`off`/`false` disables | Merges the stable external tool/ops corpus (`deploy/kb/external_w0.jsonl`: platform-neutral GPU/runtime troubleshooting, OpenAI-compatible API semantics, RAG/Agent app basics, data transfer, security, and professional GPU workflows) into the qwen3 retrieval index. Volatile platform facts stay in the internal corpus. Additive; `loadKnowledgeCorpora` degrades to platform-only if the external file is missing/bad. Boot-only; set `0` to roll back to platform-only. |
| `COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE` | default **on**; `0`/`off`/`false` disables | Exposes the read-only `SearchKnowledge` registry tool only to the verified `knowledge_qa` agent-loop lane. Generic diagnosis and unknown-intent ReAct windows cannot call it; diagnosis skills use their separate structured-claim path. Boot-only (resolved in `cmd`, frozen via `tools.SetAgenticSearchKnowledgeEnabled`; Go-package default stays off so unit tests are unaffected); set `0` to roll back. Enabled together with `COMPSHARE_EXTERNAL_KNOWLEDGE`. |
| `COMPSHARE_RAG_GROUNDED_VALIDATOR` | `1` | Enables the route-independent grounded-answer (cite + leak) validator on the agentic `SearchKnowledge` synthesis (#126). When on, the tool result carries a `[[chunk_id]]` cite protocol and a synthesis that cites no retrieved chunk (or cites an unknown one) is replaced with the canned no-evidence reply (the raw-leak guard still runs first). **Scope:** the cite-or-refuse contract applies only when SearchKnowledge surfaced evidence the agent was shown; a weak/empty-evidence turn (relevance floor dropped all hits → empty ledger) is **not** gated — there is nothing to cite, so the answer falls back to the un-gated agent reply. **Default off** — boot-only, frozen via `engine.SetGroundedAnswerValidatorEnabled`; the Go-package default stays off. Deliberately separate from `COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE`: kept off until a flag-on eval proves the agent cites at the 100%-cite / 0-leak bar; flipping it on is a separate eval-gated PR. |
| `COMPSHARE_CREATE_PREF_EXTRACTOR` | default **on**; `0`/`off`/`false` disables | Enables the create/deploy preference extractor. Boot-only, frozen via `engine.SetCreatePreferenceExtractionEnabled`; when enabled it adds one LLM pass before create/deploy image matching and only augments the preference-match input with explicit workload/image/GPU/zone preferences. It does not change router schema, create intent routing, advice gating, zone parsing, or final workflow validation. |
| `COMPSHARE_UNIFIED_CREATE` | default **on**; `0`/`off`/`false` disables | Enables the R2b unified create entry. Boot-only, frozen via `engine.SetUnifiedCreateEnabled`; the router may emit first-class `create_instance` and the engine routes it into the existing guided create / deploy-create flows. The `create_instance` path runs its own preference extraction for explicit GPU/image/source/zone/workload signals; `workload_pref` switches to the existing deploy-create matcher, while pure hardware creation only pre-fills workflow parameters. The old hardware-create rescue path has been removed, so setting this flag off disables the `create_instance` route rather than restoring the old rescue — spec-first creates then fall back to `deploy_model` / ReAct. |
| `COMPSHARE_AGENT_DETERMINISTIC_RENDER` | default **on** (2026-07-13); `0`/`off`/`false` disables | When an instance lookup (`DescribeCompShareInstance`) returns, the engine attaches a **rendered instance table** to the tool result and tells the model to write the bare placeholder `{{INSTANCE_TABLE}}` where the list belongs; the engine substitutes the real table into the finished reply. **The table never passes through the model, so it cannot mistype it.** The prose around the placeholder is still the model's own — this is not a reply template, only the enumeration is fixed. `substituteInstanceTable` is a **strict no-op** when the model omits the placeholder (the reply is returned untouched), so the flag is *never worse than off* — that is what justifies default-on. With it off the agent loop invents instances: a fabricated `另一台` row shipped to a user on 2026-07-13, and the 97-turn replay produced a phantom `uhost-…` by borrowing the prefix of an ID it had just been shown. An earlier design handed the model the finished table and asked it to reproduce the block verbatim — live, it still retyped (six rows out of ten), because *"copy this exactly"* is a request an LLM can decline by degrees. Boot-only, frozen via `engine.SetAgentDeterministicRenderEnabled`; the **Go-package default stays off** so unit tests are unaffected. Set `0` to roll back. |
| `COMPSHARE_CONTEXT_CONTINUATION` | default **on**; `0`/`off`/`false` disables | Enables the global context-decision continuation layer. Boot-only, frozen via `engine.SetContextContinuationEnabled`; short follow-ups may resume pending create/deploy frames and workflow-task frames before the normal workflow validation and confirmation card. Covered workflow-task frames currently include add-disk, resize-disk, resize-instance, scheduled shutdown, reinstall, CFS create/resize, network optimizer, custom image, and rename. This is the rollback switch for context-continuation rollout; some pre-router safety gates still remain for monitor history, diagnosis, lifecycle, and account-level billing refusal. |
| `COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP` | default **on** (2026-06-09); `0`/`off`/`false` disables | Routes `knowledge_qa` into the shared ReAct loop with a forced `SearchKnowledge` first hop. The search tool's history-aware query becomes the turn's single resolved question, and the exact evidence shown to the model is retained in one ledger. With the verifier enabled, every final knowledge answer—cited or uncited—uses the common evidence-checking exit. Inert unless agentic `SearchKnowledge` and a retriever are available. The old terminal route remains only as the `=0` rollback. |
| `COMPSHARE_KNOWLEDGE_ANSWER_VERIFIER` | default **on**; `0`/`off`/`false` disables | Enables the model-assisted semantic check at the `knowledge_qa` agent-loop exit. This is not a mathematical proof or a claim of exhaustive eval coverage: the model verdict is accepted only when local checks also verify exact answer/evidence quotes, known chunk IDs, full clause coverage, no raw-evidence leak, and no obvious polarity contradiction. Disabled is deliberately fail-closed—when evidence exists, an unchecked answer is refused rather than released. Boot-only; the Go-package default stays off for tests and embedders, while the production YAML explicitly enables it. |
| `COMPSHARE_KNOWLEDGE_QA_DISCIPLINED_SYNTHESIS` | default **on**; `0`/`off`/`false` disables | Backward-compatible name for the bounded evidence-repair gate. If the Agent draft cannot prove every substantive claim against the turn's evidence ledger, this flag permits one repair call that returns the answer and its claim-to-evidence proof together. A failed or unverifiable repair is refused honestly; disabling the flag keeps semantic verification but skips regeneration. It no longer borrows terminal RAG or retries merely to add citation punctuation. |
| `COMPSHARE_KQA_SELF_REVISION` | default **on**; `0`/`off`/`false` disables | Backward-compatible name for directness guidance inside that same proof-carrying repair call. It asks the repair to avoid unsupported hedge tails when evidence already answers the question, but does not run a separate prose rewrite and cannot bypass the common grounding proof. |
| `COMPSHARE_CONFIRM_FORM` | `1` | **Server-only.** Boot half of the editable-confirm-form double gate (create-flow 表单化, `docs/plans/2026-06-10-create-flow-form-confirm.md`): with it on AND the client opting in per turn (`SendCSAgentChat` `Features:["confirm_form_v1"]`), `confirmation` frames for `CreateInstanceWorkflow` carry a select-only `Form` (GPU/zone/image/charge-type whitelists) and `ConfirmCSAgentAction` may return `Overrides`; every edit re-runs the stock+price steps and re-confirms a refreshed card (≤3 edits). Default off — confirmation frames stay byte-identical, Overrides rejected. CLI confirm and the deploy_model saga are unaffected either way. |
| `COMPSHARE_TRACE_ENABLED` | `1` | Writes per-turn JSONL traces to `COMPSHARE_TRACE_DIR`. |
| `USE_SESSION_FACT_CONTEXT` | `1` | Injects a near-term fact cache (recent instance state, ~5min TTL) into context. Server-only wiring. **Go code default off; deploy config ships it on**. |
| `USE_REACT_RESULT_PROJECTION` | `1` | Compresses large read tool results (list endpoints) before re-feeding ReAct. **Go code default off; deploy template ships it on.** |
| `USE_REACT_HISTORY_COMPACTION` | `1` | Summarizes old turns once history exceeds the window. **Go code default off; deploy template ships it on.** |
| `COMPSHARE_INTENT_ROUTER_STRUCTURED_OUTPUT` | `json_object` \| `json_schema` | Forces the intent router to emit via `response_format`. `json_object` requests bare JSON; `json_schema` (2026-06-23) requests the typed `IntentRoute` schema (`intent.IntentRouteResponseSchema`, **non-strict** — ds-v4-flash enforces the enum/const even without `strict`; live-probed). `json_schema` only takes effect when the model capability resolves to `OutputModeJSONSchema` (`SupportsJSONSchema`), degrading to `json_object` on object-only models. **Plumbed through config.yaml but shipped OFF** — the earlier `json_object` A/B showed no schema-valid improvement (json_object carries no schema); enable `json_schema` only after the intent-router accuracy A/B validates it. |
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

- The runtime is **read-only by default in Go code** (the binary refuses mutating tools unless the runtime parser sees `COMPSHARE_ENABLE_MUTATING_TOOLS=1`). The production `deploy/conf/config.yaml` sets `agent.features.mutating_tools: true`, and `RuntimeGetenv` maps that YAML field to the parser. Destructive / L2 actions (delete, terminate) stay refused regardless (`internal/tools/safe_executor.go`). Never set the flag in tests; mutating tests use the workflow registry directly.
- Static FAQ text was removed from the ReAct prompt — platform knowledge flows only through the RAG retriever. Do not reintroduce `FAQContent` / `ReadOnlyFAQContent` injection (`internal/prompt/builder_test.go` has reverse assertions).
- Shadow QA per-round configs under `eval/shadow_qa/**/agent.yaml` and `.env` files are git-ignored and contain real keys — never commit anything matching those globs.
- When adding planner examples, group by intent and record a one-line source for each example; tests in `internal/intent/planner_prompt_test.go` enforce grouping/tool/intercept consistency.
- `SecurityToken` must be included in API signing params before computing the HMAC-SHA1 signature. See `internal/tools/README.md` §6 for the six common pitfalls.
