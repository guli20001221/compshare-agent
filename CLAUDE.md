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

**Config is YAML (`deploy/conf/config.yaml`).** Runtime flags have typed fields under `agent.features` / `agent.retrieval` / `agent.trace` (see `internal/config/runtime.go`). The deploy file also carries secrets directly: LLM key, STS service AK/SK, role URN, and PostgreSQL DSN. The env-var names below are still the historical parser names in code, but deployment must set the matching YAML fields instead of exporting environment variables. The default answer path uses **gpt-5.6-terra** (ModelVerse GPT-5 Codex) with qwen3 RRF retrieval.

**Two-key setup (answer vs retrieval):** `gpt-5.6-terra`'s ModelVerse key is authorized only for terra, **not** for `qwen3-embedding-8b` / `qwen3-reranker-8b`, so the answer model and the retrieval stack use separate keys. `agent.llm.api_key` is the answer key (terra); `agent.retrieval.api_key` is the qwen3 embed/rerank key (`RuntimeGetenv` exposes it as `MODELVERSE_API_KEY`, which `cmd/trace.go::modelverseAPIKeyFromEnv` reads before `LLM_API_KEY`). Both are inlined in `deploy/conf/config.yaml` per deploy convention — like the platform key they live in the repo's git history, so rotate them with the standing key rotation. When `agent.retrieval.api_key` is empty, embed/rerank inherit `agent.llm.api_key` (single-key mode — the pre-terra behavior, e.g. for a `deepseek-v4-pro`/`-flash` answerer whose key also does qwen3).

| Var | Values | Effect |
|---|---|---|
| `COMPSHARE_ENABLE_MUTATING_TOOLS` | `1` | Enables start/stop/reboot/reset-password/create. Default off — read-only mode. |
| `USE_KNOWLEDGE_RETRIEVAL` | `curated` (default), `off` | Wires the RAG retriever into the engine. Combine with `RAG_RETRIEVAL_MODE`. |
| `RAG_RETRIEVAL_MODE` | `qwen3_rrf` (default), `bm25_only`, `hybrid_cosine`, `hybrid_rerank`, `qwen3_full` | Picks the retrieval pipeline. Hybrid/qwen3 modes require `MODELVERSE_API_KEY` or `LLM_API_KEY` and the matching pinned sidecar under `deploy/kb/`. |
| `RAG_HYBRID_ENABLED` | `1` | Legacy switch; only consulted when `RAG_RETRIEVAL_MODE` is unset. |
| `COMPSHARE_EXTERNAL_KNOWLEDGE` | default **on**; `0`/`off`/`false` disables | Merges the stable external tool/ops corpus (`deploy/kb/external_w0.jsonl`: platform-neutral GPU/runtime troubleshooting, OpenAI-compatible API semantics, RAG/Agent app basics, data transfer, security, and professional GPU workflows) into the qwen3 retrieval index. Volatile platform facts stay in the internal corpus. Additive; `loadKnowledgeCorpora` degrades to platform-only if the external file is missing/bad. Boot-only; set `0` to roll back to platform-only. |
| `COMPSHARE_CONFIRM_FORM` | `1` | **Server-only.** Boot half of the editable-confirm-form double gate (create-flow 表单化): with it on AND the client opting in per turn (`SendCSAgentChat` `Features:["confirm_form_v1"]`), `confirmation` frames for `CreateInstanceWorkflow` carry a select-only `Form` (GPU/zone/image/charge-type whitelists) and `ConfirmCSAgentAction` may return `Overrides`; every edit re-runs the stock+price steps and re-confirms a refreshed card (≤3 edits). Off → confirmation frames stay byte-identical, Overrides rejected. CLI confirm and the deploy_model saga are unaffected either way. **Go code default off; deploy config ships it on.** |
| `COMPSHARE_GUIDED_CREATE` | `1` | **Server-only.** Boot half of the guided GPU create order, paired with the client's per-turn `guided_create_v1` opt-in. Requires `COMPSHARE_CONFIRM_FORM=1` — `cmd/server.go` logs a warning and treats guided create as off otherwise. **Go code default off; deploy config ships it on.** |
| `COMPSHARE_FORCED_KNOWLEDGE_HOP` | `1` | The engine performs one retrieval on the user's own words before the Agent's first model call of the turn and injects the result as an ordinary tool observation, instead of leaving the decision to search up to the model (`internal/engine/forced_knowledge_hop.go` documents the measurement that motivated it: a complaint phrased as 好贵 searched 0/5, the same topic phrased as a question 5/5). The planner expands that query rather than merely de-referencing it, and keeps the user's raw words as the last query. Boot-only, frozen via `engine.SetForcedKnowledgeHopEnabled`. **Off everywhere since 2026-08-01** (Go default off; `deploy/conf/config.yaml` deliberately **omits** the key rather than setting `false` — see the rollback note below). Base rates over 4348 real questions: the statement-form-needing-knowledge shape it targets is 3.4% of traffic, while 43.7% are questions that retrieve unprompted anyway; a 6-shape x N=5 live A/B measured 4.7x SearchKnowledge calls and +39.6% wall clock for identical work. Narrowing the trigger would need a pre-model classifier that no longer exists (P6 removed the intent router) and would rebuild the layer the canonical-transcript program is removing. Rollback is `COMPSHARE_FORCED_KNOWLEDGE_HOP=1` **plus a restart** — the flag is boot-only (`engine.SetForcedKnowledgeHopEnabled` freezes it at startup), so this avoids editing YAML or shipping code, not a process restart. It works at all only because the deploy config omits the key: `putBoolEnv` records a non-nil `*bool` unconditionally and `RuntimeGetenv` consults that overlay before `os.Getenv`, so an explicit YAML `false` would out-rank the env var (pinned by `cmd/runtime_overlay_test.go`). |
| `COMPSHARE_CANONICAL_TRANSCRIPT` | `1` | Replays a prior turn's assistant `tool_calls` and tool results to the model instead of deleting them and paraphrasing them back through `TaskSnapshot` / `ConversationDigest` / `RecentFacts` / `VerifiedKnowledge` / the context card. The record itself (`messages.metadata` → `agent_transcript_v1`) is produced and persisted **regardless of this flag** — it is a shadow write with no model-visible effect — so flipping the flag on finds history already there to project. Off means the assembled prompt is byte-identical to before. Bounded at the producer (per-message 6000 runes with `truncated`/`orig_runes` markers, whole tool rounds shed at 40000, 256KB envelope guard) and charged against the same `maxReplayedHistoryRunes` budget as the exchange it belongs to. Legacy path only: durable `CommitTurn` and `hashCommit` are deliberately untouched, because hashing metadata the commit does not write would make the fingerprint attest to a column the row lacks. Boot-only, frozen via `engine.SetCanonicalTranscriptEnabled`. **Default off in both code and deploy config.** |
| `COMPSHARE_RAG_DOMAIN_MATCH_GUARD` | `1` | Wrong-domain REFUSE arm (#5 cite-relevance). Default off in both code and deploy config. |
| `COMPSHARE_DURABLE_TURNS` | `1` | **Server-only.** Durable turn persistence path. Default off; unknown values are a hard error at boot, not a warning (`cmd/server.go`). |
| `COMPSHARE_TRACE_ENABLED` | `1` | Writes per-turn traces. Sink is chosen by `COMPSHARE_TRACE_SINK` (`file` \| `mysql` \| `both`); `file` writes JSONL to `COMPSHARE_TRACE_DIR`. |
| `COMPSHARE_SSH_OPS` | `1` | Enables the consent-gated, **read-only in-instance SSH-ops** lane (`DiagnoseInstanceInternals`). The model self-elects the tool; entry is gated by an authorization card; a Python Agent-SDK harness SSHes into the box and runs only read-only commands (mutating/destructive refused), streaming each command's metadata as a live activity event and returning a Chinese root-cause verdict. Default off. **Server path requires a non-static STS provider** (`agent.sts.service_ak/sk`; static AK/SK is refused — no per-tenant instance scoping, INV-12) and a DB (fail-closed `ssh_ops_audit`, migration 0011). It runs on the **current non-durable transport** (WS/SSE via `chatStream`, which carries the confirm card + step stream); it does **not** require `COMPSHARE_DURABLE_TURNS` — the lane is read-only, so a mid-diagnosis disconnect just ends the probe and the user retries (durable, when on, additionally makes the harness survive a disconnect). CLI path (`cli`) needs only this flag; it uses an in-memory audit. The harness uses ModelVerse's Anthropic-compatible endpoint directly; non-bool settings live under `agent.ssh_ops` (`harness_path`, `base_url`, optional `api_key`, `python`, `model`, `timeout`). An empty SSH-ops key inherits `agent.llm.api_key`. YAML `agent.ssh_ops.enabled: false` wins over the env. **`agent.ssh_ops.allow_writes`** (default false) is a separate gate that lets the harness EXECUTE the `mutating` tier instead of refusing it, so the agent repairs rather than only describes the repair; it rides the same one-shot stdin handshake as the credential. Destructive commands and command substitution / multi-line input stay refused in BOTH modes — the shape gate is the prompt-injection firewall (classify() only sees the literal string), not part of the read-only policy. Flipping it also switches the consent-card label, the tool description and the audit `phase` (`read_only` -> `read_write`), all off `tools.SetInstanceOpsWritesEnabled`, frozen once in `cmd/instance_ops.go`: a card that says 只读排查 while the harness writes is consent that was never given. |
| `USE_SESSION_FACT_CONTEXT` | `1` | Injects a near-term fact cache (recent instance state, ~5min TTL) into context. Server-only wiring. **Go code default off; deploy config ships it on**. |
| `USE_REACT_RESULT_PROJECTION` | `1` | Compresses large read tool results (list endpoints) before re-feeding ReAct. **Go code default off; deploy template ships it on.** |
| `USE_REACT_HISTORY_COMPACTION` | `1` | Summarizes old turns once history exceeds the window. **Go code default off; deploy template ships it on.** |
| `MYSQL_DSN` | DSN string | PostgreSQL libpq URL (env var name kept for compat). Required by `compshare-agent server`; ignored by `compshare-agent cli`. |

Unknown values for any of the above are logged as warnings and treated as off — do **not** silently coerce them. The exception is `COMPSHARE_DURABLE_TURNS`, where an unknown value fails the boot outright.

**Inert config keys.** `agent.features.skill_executor` / `skill_executor_diagnosis_pilots` still parse and still emit `USE_SKILL_EXECUTOR` / `USE_SKILL_EXECUTOR_DIAGNOSIS_SKILLS`, but **nothing reads those overrides** — the body-driven skill executor they gated no longer exists. Setting them changes no behavior; they are kept only so an old config file does not fail to load.

**STS credentials are not env vars in the current deploy.** `agent.sts.service_ak` / `service_sk` / `default_role_urn` are written literally into `deploy/conf/config.yaml`. The loader does support `${VAR}` substitution (`internal/config/config.go`), so `COMPSHARE_SERVICE_PUBLIC_KEY` / `COMPSHARE_SERVICE_PRIVATE_KEY` / `COMPSHARE_DEFAULT_ROLE_URN` still work as placeholder names — but the shipped config uses none of them.

## Knowledge base — pinned digests

`deploy/kb/` holds the customer-safe FAQ corpus and embedding sidecars. All three artifacts are byte-pinned by LF-normalized SHA256 in `internal/knowledge/corpus_digest.go`:

- `stage2b_w0.jsonl` → `CorpusDigestExpected`
- `embeddings_<digest>.jsonl` (text-embedding-3-large, 3072d) → `EmbeddingDigestExpected`
- `embeddings_<digest>_qwen3-embedding-8b.jsonl` (qwen3, 4096d) → `EmbeddingDigestExpectedQwen3`

The loader **refuses to start** if any pin mismatches. When the corpus changes, regenerate **both** sidecars and update **all three** digest constants in the same change. See `deploy/kb/README.md` for the rebuild commands and PR #113/#114 for the 8-step flow.

## Architecture

### Entry path
`cmd/agent.go` (CLI loop) → `engine.Engine.Init()` → per-turn `Engine.Chat()`. HTTP、CLI、缓存冷建和 durable rehydrate 都通过 `engine.NewSession` 创建同一个中心 AgentRuntime。`cmd/trace.go` 只负责检索、渲染和 trace 等运行依赖，不再创建独立 Router。

### Engine (`internal/engine/`)
Runs a single central agent loop (`maxReActRounds=16`, `maxHistoryMessages=120`) with per-turn budgets: `maxReadExpensiveCallsPerTurn=30` for read tools, and for retrieval `maxSearchKnowledgeCallsPerTurn=4` (agent decisions to search) against `maxRetrievalQueriesPerTurn=8` (actual retrievals — one decision fans out into several queries). The Agent receives compiled `AgentContext`, selects read capabilities or proposes writes, and observes tool results in the same loop. Read capabilities may return deterministic evidence/rendering after the Agent selects them. Mutating operations remain blocked unless `COMPSHARE_ENABLE_MUTATING_TOOLS=1` and must pass resolver, confirmation, permission and action-journal gates.

Force-tool / hard-block priority chain (highest first) is documented inline in `engine.go` and **must be kept in sync** when adding new force paths: unsupported-historical-monitor canned reply > monitor-recall force tool (the account-billing-unsupported keyword hard-block was removed 2026-06-10 — that intent now dispatches to ReAct). Forced `tool_choice` is guarded at **runtime**, not by a per-model table. `llm.Client.Chat` retries once with `auto` when the provider rejects a forced tool_choice in thinking mode, and reports the silent degrade via `ChatResponse.ForcedToolChoiceDegraded` (`internal/llm/client.go`). A new force-tool path should read that flag and fall back, rather than pre-checking a capability. The static `llm.Capability` matrix that used to serve this was **deleted 2026-07-31**: it had no production reader, no production code sets `ChatRequest.ToolChoice`, its `ds-v4-flash` row had already needed a manual re-probe to flip `false`→`true` (2026-05-08 → 2026-06-08), and `gpt-5.6-terra` — the default answerer since 2026-07-24 — never had a row at all, so `LookupCapability` returned the zero value for it. The retry cannot go stale on a model nobody re-probed; the table could, and had.

### Read capability catalog (`internal/capability/`, `internal/intent/`)
Model-visible read capabilities live in `internal/capability/read_*.go`. Each owns its own typed request struct, its field contract (a `schemaNode` in `field_contract.go` that is the **single source** for the model tool schema, runtime enum/minimum validation, and the consistency test), its handler and its renderer; `ReadDefinitions()` (`read_catalog.go`) is the catalog. The engine dispatches a read tool through `executeConcreteReadCapability` → `capability.MigratedRead(action)` → `RegisteredRead.Run` — there is **no separate route registry**. The legacy intent-based route stack (`internal/routing`, `cmd/routegen`, the `route.yaml` manifests, `DispatchRoute` / `IsRoutingIntent` / `RoutingPromptFragments`, `RouteHandlerForKey`) was **physically deleted in P6**: adding a read capability now means authoring a `ReadCapabilitySpec` vertical, not a route manifest, and nothing generates or reads `routing.GeneratedRoutes()` anymore. The diagnosis chains are now a hand-written registry (`internal/diagnosis/registry.go`); the old `internal/skills/<name>/SKILL.md` + `cmd/skillgen` generate-from-frontmatter path was deleted in P6 along with the route stack (no `internal/skills`, `cmd/skillgen`, or `SKILL.md` remains in the tree). The legacy `capabilityRegistry` / `capability_registry.go` / `IsCapabilityIntent` / `DispatchCapability` were retired in #115; **capability** now names a typed read/action Capability (`internal/capability`) or model capability (`llm.Capability`, e.g. `supportsObjectToolChoice`).

### Workflow engine (`internal/workflow/`)
Multi-step mutating flows (create/start/stop/reboot/reset-password/rename) live as `*Workflow` types. Confirmation is delivered via the `engine.ConfirmFunc` callback (CLI implementation in `cmd/agent.go::cliConfirm`).

### Knowledge / RAG (`internal/knowledge/`)
Retriever modes are listed above. The production Agent's **system prompt** is built from the Go segments in `internal/prompt/segments.go` (assembled by `builder.go`); there is no shared Go/Python prompt directory. The old terminal-RAG `internal/prompt/rag_system_segments/` snippets and the Python `evaluate_answers` answer-grading harness that read them were removed — RAG is now an in-loop tool the central Agent calls, not a separate terminal-RAG prompt, and answer-quality eval should be re-derived from real HTTP/WebSocket traces rather than a second Python prompt. Reranker / embedder timeouts are knobbed by `RAG_HYBRID_TIMEOUT_MS` / `RAG_RERANKER_TIMEOUT_MS`.

### Diagnosis (`internal/diagnosis/`)
Read-only diagnostic chains. **`DiagnoseBilling` is the only model-visible one** — `registeredDiagnosisActions` and `chainRegistry` in `registry.go` both hold exactly that one entry, so an unadvertised diagnosis name cannot resolve to a chain (`TestDiagnosisRegistryHasNoUnadvertisedChains`; model-invisible ≠ unreachable). The init-failure, GPU-not-detected, image-issue, and port/firewall chains were deleted outright in the pre-P7 convergence (no diagnosis value, or superseded by the central Agent gathering evidence via `SearchKnowledge` + `DescribeCompShareInstance`).

The SSH chain is the exception worth knowing: `SSHFailureChain` / `SSHFailureChainWithDescribeResult` still exist and still run, but **not as a `DiagnoseSSH` tool** — `internal/capability/read_instance_access.go` constructs the chain directly, so SSH reaches the model as `ReadCapability_instance_access`. A `DiagnoseSSH` name grants nothing (`TestVisibleRegistryForSubset_DiagnosisToolsOnly`).

Boundary rule baked into prompts: read-only self-check commands may be suggested as user actions; commands that change environment must be marked as **optional fixes**, never auto-executed. Source-of-truth notes:
- The SSH precheck is **cloud-side**, not an end-to-end reachability test: it does not open a TCP connection or enter the guest OS. It must match the exact requested `UHostId` (`instanceAccessHostForID`); a non-empty `SshLoginCommand` counts as an endpoint only when its host/port are complete and agree with `IPSet` (UHost) or the internal-port-23 `TcpForwards` mapping (Pod, `podTCPForwardPresent`). `DescribeCompShareSoftwarePort` exposes image application ports, not SSH.
- Current upstream initialization state is `Initializing`; legacy `Install` is accepted only for response compatibility. CPU/memory/system-disk monitoring values are risk signals, not causal proof. Missing monitoring data must surface as "无法确认", never as 0%/healthy.

### Observability (`internal/observability/`)
`observability.Writer` writes one JSONL line per turn. `cliTraceRecorder` in `cmd/trace.go` is the bridge that wires retrieval, renderer, tool, workflow and token-usage observations into the writer. Retention: `DefaultTraceRetentionDays`, cleaned on each run. Historical router fields remain readable for old trace records but are no longer produced by the current runtime.

### Other notable boundaries
- `internal/security/secret_boundary.go` + `internal/sanitizer/` — keep redaction logic centralized; do not inline new redaction in tools.
- `internal/policy/leakage.go` — citation-leakage guards used by the cited-strip pass in the engine.
- `internal/governance/ratelimit.go` — QPS/daily limits live in `agent.rate_limit` config and are enforced for LLM, mutating, and read-expensive call classes.
- `internal/entity/` — only Go package run with `-race` in CI; concurrent registry access is a known concern there.
- `internal/ocr/` (screenshot understanding, server/WS-only) — when `SendCSAgentChat` carries an `Image`, a Qwen3-VL call (`agent.ocr.model`, e.g. `qwen3-vl-flash`; empty = disabled) interprets the screenshot to **structured text** that is injected as context (it is NOT plain OCR, and the raw image never reaches the main model). The vision prompt is `ocr.DefaultPrompt`, overridable via `agent.ocr.prompt` (empty/whitespace = default, never an empty instruction). Trust boundary: recognized screenshot text is **untrusted reference context** — fenced via `engine.WrapScreenshotContext` (the single producer for both the live turn and the persisted/rehydrated copy), interprets-but-does-not-prescribe-fixes, runs through `RedactPII`, and feeds only conversation history (never routing/force-tool/hard-block, which use the raw user message). It must never auto-drive a mutating action; the confirmation gate / `COMPSHARE_ENABLE_MUTATING_TOOLS` remains the hard stop.

## HTTP service

`compshare-agent server` runs the HTTP gateway alongside the CLI; both create the same central AgentRuntime via `engine.NewSession` and share the engine/knowledge core (the standalone Planner/intent-router was deleted in P6).

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
- The intent-router Planner and its grouped prompt examples were removed in P6 (`internal/intent/planner.go` and `planner_prompt_test.go` no longer exist). The central Agent's system prompt is assembled from `internal/prompt/segments.go`; change it behind the prompt-snapshot tests (see `docs/dev/prompt-change-checklist.md`).
- `SecurityToken` must be included in API signing params before computing the HMAC-SHA1 signature. See `internal/tools/README.md` §6 for the six common pitfalls.
