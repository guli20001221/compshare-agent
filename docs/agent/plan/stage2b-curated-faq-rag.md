---
status: draft for review
parent: stage2-intent-planner.md
stage: Stage 2B
created: 2026-05-09
primary_baseline: deepseek-v4-flash
---

# Stage 2B Curated FAQ / Runbook RAG

## Purpose

Stage 2A proved the deterministic handler path for resource and monitor
queries. Stage 2B adds the first knowledge layer for user-facing platform
questions: curated FAQ and runbook retrieval.

The goal is to answer common user-group and product-usage questions without
keeping the full FAQ permanently inside every system prompt, while preserving
the Stage 2 rule that API facts outrank knowledge text.

This is a narrow first slice:

- curated customer-safe knowledge only;
- no raw group-chat ingestion;
- no automatic FAQ mining;
- no default behavior change until an explicit gate is enabled;
- no fallback to ungrounded FAQ-like prose when retrieval misses.

## Current Starting Point

The repo currently injects `internal/prompt.FAQContent` into every system prompt
through `prompt.BuildSystem`. That static FAQ covers 11 topics:

- image selection;
- login methods;
- firewall and ports;
- cloud disk billing;
- public model library;
- network acceleration;
- no-GPU mode;
- billing and recycling rules;
- model API packages;
- deployment practices;
- account management.

This is useful but has three limitations:

- every turn pays the FAQ token cost even for resource or monitor questions;
  current test output reports `FAQContent: 3138 runes, ~2092 tokens
  (estimate)`;
- trace only records the final LLM interaction, not which knowledge item
  grounded the answer;
- user-group FAQ updates require editing prompt text, not updating a versioned
  knowledge corpus.

Stage 2B starts replacing "always inject all FAQ" with "retrieve a small set of
versioned, customer-safe chunks when the planner says the question is
knowledge". The first slice prioritizes grounding and traceability. Removing
the static FAQ from every ReAct prompt is a follow-up once the gated retrieval
path has smoke evidence.

## Scope

### Eligible Intents

| Intent | Stage 2B behavior |
| --- | --- |
| `knowledge_qa` | Retrieve curated FAQ/runbook chunks and answer from those chunks only. |
| `mixed_diagnosis_kb` | Keep runtime path on existing ReAct/diagnosis in this ticket; only make retrieval contract ready. |
| `mixed_billing_kb` | Keep runtime path on existing ReAct/diagnosis in this ticket; only make retrieval contract ready. |

The first implementation should cut over only `knowledge_qa`. Mixed intents
need API + KB conflict handling and are a later Stage 2B follow-up.

### Existing Local Knowledge Tools

The repo already has local knowledge tools such as `GetGPUSpecs` and
`GetGPURecommendation`. Stage 2B curated FAQ does not replace them in the first
slice.

Rules:

- GPU specification / GPU recommendation questions stay on the existing
  `knowledge_local` tool path unless a later reviewed PR migrates them.
- Curated FAQ handles platform usage and runbook questions such as image
  categories, login, ports, no-GPU mode, billing rules, and account management.
- If planner output is ambiguous between GPU specs and FAQ, the first slice
  should fall back to the existing path rather than guessing.

### Ineligible Content

The curated corpus must not include:

- raw user-group chat logs;
- internal-only operations notes;
- account-specific data;
- private keys, tokens, passwords, SSH commands, IPs, balances, charge amounts,
  or real user identifiers;
- hardcoded dynamic prices or capacity claims that should come from APIs or the
  console.

User-group questions may be included only after manual curation into
customer-safe FAQ/runbook chunks. Raw group-chat retrieval remains Stage 2C.

## Feature Gate

Introduce an explicit opt-in gate:

```text
USE_KNOWLEDGE_RETRIEVAL=curated
```

Rules:

- empty / unset means current behavior;
- unknown values are ignored with a warning, not a panic;
- `curated` enables the Stage 2B retriever for `knowledge_qa`;
- the gate must not enable raw chat retrieval;
- the gate must not change account hard-block behavior.

Corpus path resolution:

- `COMPSHARE_KNOWLEDGE_CORPUS` overrides the corpus path when set;
- otherwise the CLI loads the bundled `deploy/kb/curated_faq.jsonl`;
- missing or invalid corpus files disable retrieval with a warning and must not
  block the ReAct fallback path.

When both planner cutover and curated retrieval are enabled, the engine must use
the same planner call that already feeds Phase 1 cutover. Do not add a second
planner call solely for knowledge retrieval.

## Knowledge Chunk Contract

Knowledge data lives in a versioned customer-safe corpus. The first storage
format is JSONL, and each chunk must map to this schema:

```json
{
  "chunk_id": "faq-billing-001",
  "kb_version": "kb-curated-2026-05-09",
  "source_type": "faq",
  "product_area": "billing",
  "acl": "customer_safe",
  "valid_from": "2026-05-09",
  "valid_to": null,
  "confidence": "high",
  "title": "按量实例关机后是否继续收费",
  "question_patterns": ["关机后还收费吗", "关机后磁盘收费吗"],
  "content": "按量实例关机后 GPU/CPU/内存停止计费，但额外数据盘继续收费。具体金额以控制台财务中心为准。",
  "source_url": "https://www.compshare.cn/docs/..."
}
```

Required fields:

- `chunk_id`;
- `kb_version`;
- `source_type`;
- `product_area`;
- `acl`;
- `confidence`;
- `title`;
- `content`.

Rules:

- `acl` must be `customer_safe` for every retrievable chunk in Stage 2B.
  The field is kept now because Stage 2C may add lower-trust internal or
  ops-chat sources; Stage 2B loaders must still reject anything except
  `customer_safe`.
- `kb_version` must be stable for a release and recorded in trace.
- `content` must avoid dynamic numeric facts unless it explicitly points to the
  API or console as authority.
- `question_patterns` must contain at most 20 entries, each at most 200 runes.
- `content` must be at most 4000 runes.
- Oversized chunks must fail corpus loading with a clear row-level error.
- `confidence` is a renderer hint, not a retrieval override. The first slice
  accepts `high` and `medium`; `low` chunks are loaded but not returned unless a
  later reviewed change adds low-confidence phrasing.
- `source_url` is optional, but `source_type` and `title` are required so the
  renderer can cite the type of source without exposing raw internal material.
- Any chunk derived from user-group discussion must be rewritten as curated FAQ
  text and marked `source_type="faq"` or `source_type="runbook"`, not
  `ops_chat`.

## Retriever Contract

The first retriever is deterministic, in-process, and JSONL-backed. A vector
database is not required for this ticket. YAML is deferred until there is a
specific authoring workflow that needs it.

Inputs:

- user question;
- planner intent;
- optional product area inferred by the engine from the question and matched
  chunk metadata; the first engine implementation uses a narrow curated
  keyword map for the bundled product areas and does not consume planner
  `scope`; do not grow the `Plan.Retrieval` schema in the first slice;
- current time for validity filtering.

Outputs:

```go
type RetrievalResult struct {
    Enabled   bool
    KBVersion string
    Hits      []KBChunk
    Empty     bool
}
```

Required behavior:

- filter out chunks where `acl != customer_safe`;
- filter out chunks outside `valid_from` / `valid_to`;
- rank by deterministic weighted keyword overlap:
  - question pattern match = 4 points, capped once per chunk;
  - title match = 3 points;
  - product area match = 2 points;
  - content match = 1 point;
  - `confidence=high` wins ties over `medium`.
- matching in the first slice is exact substring matching in either direction,
  not semantic embedding search; chunk authors must enumerate common user
  phrasings in `question_patterns`, and the smoke artifact should disclose any
  paraphrase misses.
- return at most 3 chunks;
- default threshold is 2 points, supplied through constructor options and not an
  environment variable in the first slice;
- return `Empty=true` when no hit passes the threshold;
- never synthesize a chunk from the user question.

If embeddings are introduced later, they must preserve the same input/output
contract.

## Planner Contract

The first Stage 2B slice is driven by `intent == knowledge_qa` plus
`USE_KNOWLEDGE_RETRIEVAL=curated`. It does **not** require the planner to set
`Plan.Retrieval.Enabled=true`.

Rationale:

- Stage 2A validation currently rejects `Plan.Retrieval.Enabled=true`;
- trace already has a separate `retrieval.enabled` field for the actual engine
  retrieval call;
- keeping planner retrieval disabled avoids widening validator semantics before
  the first curated FAQ slice proves useful.

Contract:

- `Plan.Retrieval.Enabled` must stay false until a reviewed Stage 2B+ change
  relaxes validator semantics.
- `trace.retrieval.enabled` records whether the engine actually called the
  retriever.
- Prompt updates must teach:
  - clear platform usage / FAQ questions -> `knowledge_qa`;
  - API-grounded resource or monitor questions -> existing API intents;
  - diagnosis + FAQ questions -> `mixed_diagnosis_kb` for observability, but no
    Stage 2B runtime handler yet;
  - billing-specific FAQ + instance facts -> `mixed_billing_kb` for
    observability, but no Stage 2B runtime handler yet;
  - account totals / balances / ledgers -> `billing_account_unsupported` and
    engine hard-block remains authoritative.

If a later Stage 2B+ follow-up wants planner-owned retrieval flags, it must
change `intent.ValidatePlan` explicitly and update this document.

## Runtime Flow

```text
user message
  -> permanent hard-blocks
      -> canned reply if matched; no planner or retrieval needed
  -> one planner call when any planner-backed feature gate is enabled
      -> Phase 1 cutover dispatcher
          -> resource_info / monitor_query eligible -> deterministic handler
          -> not eligible -> continue
      -> Stage 2B retrieval dispatcher
          -> knowledge_qa + USE_KNOWLEDGE_RETRIEVAL=curated -> retrieve
              -> hit -> grounded FAQ renderer
              -> miss -> fixed FAQ miss reply
          -> otherwise -> continue
      -> existing ReAct path
```

The retrieval dispatcher runs after the Phase 1 resource/monitor dispatcher and
reuses the same planner result. It must not call the planner again.

Additive `planner.cutover_status` values:

| Value | Meaning |
| --- | --- |
| `dispatched_retrieval` | Stage 2B accepted a `knowledge_qa` plan and called the curated retriever. |
| `fallback_retrieval_miss` | Stage 2B called the retriever but no chunk passed threshold. |
| `fallback_retrieval_disabled` | Planner emitted `knowledge_qa`, but the Stage 2B gate was disabled. |

Existing Phase 1 values keep their meaning. If both dispatchers are disabled or
not applicable, `planner.cutover_status` remains empty.

The Stage 2B knowledge path must not call CompShare APIs. If a question needs
live facts, the planner should classify it as API-backed or mixed; this first
ticket falls back to the existing path for mixed cases.

## Renderer Contract

The knowledge renderer receives only:

- the user question hash or sanitized text needed for wording;
- retrieved chunks;
- retrieval metadata.

It must:

- answer only from retrieved chunks;
- say when the answer is based on platform FAQ/runbook knowledge;
- point dynamic prices, capacity, billing totals, balances, and instance state
  to the console or API-backed paths;
- avoid citing unavailable raw group messages;
- return a fixed fallback when retrieval misses.

The fixed miss reply is:

```go
const KnowledgeMissReply = "\u6682\u672a\u627e\u5230\u5339\u914d\u7684\u5e73\u53f0FAQ\u6216\u64cd\u4f5c\u6307\u5357\uff0c\u8bf7\u6362\u4e2a\u95ee\u6cd5\uff0c\u6216\u4f7f\u7528\u63a7\u5236\u53f0\u5e2e\u52a9\u6587\u6863\u67e5\u8be2\u3002"
```

That is: "暂未找到匹配的平台FAQ或操作指南，请换个问法，或使用控制台帮助文档查询。"

It must not:

- invent product rules not in chunks;
- expose chunk internals beyond safe title/source type/source URL;
- override API facts from resource, monitor, billing, or diagnosis handlers.

## Trace Requirements

`trace.v0.1` already has:

```json
"retrieval": {
  "enabled": false,
  "kb_version": "",
  "hits": 0
}
```

Stage 2B must populate:

| Field | Rule |
| --- | --- |
| `retrieval.enabled` | `true` only when the curated retriever was called. |
| `retrieval.kb_version` | Corpus version used for the call. Empty only when disabled or unavailable. |
| `retrieval.hits` | Number of returned chunks after threshold filtering. |
| `outcome.kb_conflict_count` | Always `0` in the first knowledge-only slice; mixed API+KB conflict handling is a follow-up. |

Do not add raw chunk text to trace. If later dashboards need hit attribution,
add hashed `chunk_id` values in a trace schema extension after review.

`trace.retrieval.enabled` is the engine's actual retrieval-call decision, not a
mirror of `Plan.Retrieval.Enabled`.

## Safety / Hard-Block Rules

Permanent engine hard-blocks remain before planner and retrieval:

- account balance / total bill / transaction ledger;
- destructive operations;
- secret or prompt-injection patterns when implemented.

For account-level billing questions, curated FAQ may explain general billing
rules only if the hard-block did not match. It must not answer account-specific
totals, balances, or ledgers.

If hard-block matches, no retrieval call is needed and trace must record the
engine hard-block outcome as it does today.

## Implementation Plan

### Commit 1: Corpus Schema and Loader

Files:

- `internal/knowledge/chunk.go`
- `internal/knowledge/loader.go`
- `internal/knowledge/testdata/curated_faq.jsonl`
- `internal/knowledge/loader_test.go`

Scope:

- define `KBChunk`;
- load JSONL corpus;
- validate required fields;
- reject non-`customer_safe` chunks by default;
- reject oversized `question_patterns` and `content`;
- expose corpus `KBVersion`.

Acceptance:

- invalid JSONL row fails with row number;
- missing required fields fail;
- oversized question patterns or content fail;
- non-customer-safe chunks are not retrievable;
- no raw secret-looking values appear in test corpus;
- existing `internal/prompt.FAQContent` remains unchanged until cutover wiring.

### Commit 2: Deterministic Retriever

Files:

- `internal/knowledge/retriever.go`
- `internal/knowledge/retriever_test.go`

Scope:

- implement deterministic ranking over title, question patterns, product area,
  and content;
- support top-K and score threshold;
- no LLM calls;
- no vector database.

Acceptance:

- billing disk/offline question retrieves billing chunk;
- image question retrieves image chunk;
- unrelated question returns empty;
- expired chunk is ignored;
- ranking is stable across map/file iteration order;
- threshold defaults to 2 points and is constructor-configurable in tests.

### Commit 3: Knowledge Renderer

Files:

- `internal/knowledge/renderer.go`
- `internal/knowledge/renderer_test.go`

Scope:

- render a grounded Chinese answer from retrieved chunks;
- render a fixed miss reply;
- do not call APIs or LLM.

Acceptance:

- answer uses only retrieved chunk content;
- dynamic prices are phrased as console/API authority, not hardcoded;
- source title/type can be included;
- miss reply is deterministic and does not hallucinate a FAQ.

### Commit 4: Engine Opt-In Wiring

Files:

- `internal/engine/engine.go`
- `cmd/agent.go`
- focused tests in existing engine/cmd test files.

Scope:

- parse `USE_KNOWLEDGE_RETRIEVAL=curated`;
- when planner emits valid `knowledge_qa`, call curated retriever and renderer;
- reuse the existing planner result shared with Phase 1 cutover; do not add a
  second planner call;
- keep `prompt.FAQContent` injection unchanged for default and old ReAct paths.
  Removing or conditionally disabling static FAQ injection is a separate
  Stage 2B+1 follow-up after smoke proves no regression;
- populate trace retrieval fields;
- keep default behavior unchanged;
- keep mixed intents on existing path.

Acceptance:

- gate unset: current behavior and tests unchanged;
- hard-block happens before retrieval;
- exactly one planner LLM call occurs when Phase 1 cutover and Stage 2B
  retrieval gates are both enabled;
- `knowledge_qa` + hit returns grounded answer without API tool calls;
- `knowledge_qa` + miss returns the fixed miss reply;
- `billing_account_unsupported` still returns canned console guidance;
- trace records `retrieval.enabled=true`, `kb_version`, and hit count;
- no raw chunk text is written to trace.

### Commit 5: Smoke Artifact

Files:

- `eval/capability/2026-05-09-stage2b-curated-faq-smoke.md`

Scope:

- run a small smoke with `deepseek-v4-flash`;
- no raw transcripts or traces committed;
- report only sanitized question categories and trace-derived outcomes.

Acceptance:

- at least one image/login/port FAQ hit;
- at least one billing-rule FAQ hit that does not answer account-specific
  totals;
- billing-rule smoke asserts no currency amount pattern such as
  `\d+(\.\d+)?\s*(元|RMB|USD)` appears in the answer, and that the answer
  includes a console/API authority phrase;
- at least one retrieval miss;
- at least one account hard-block case proves retrieval was bypassed;
- `go test ./... -count=1` passes;
- `scripts/secret_scan.ps1` passes.

## Review Checklist

- Does any retrieval path turn on by default?
- Can raw group chat enter the corpus?
- Can retrieval answer account-specific billing totals or balances?
- Can knowledge text override API facts?
- Are retrieval misses deterministic and non-hallucinated?
- Are trace fields populated without raw chunk text?
- Is `deepseek-v4-flash` the primary smoke baseline?
- Are mixed intents explicitly deferred rather than partially implemented?

## Follow-Up After First Slice

- Mixed API + KB handlers with conflict matrix.
- Optional vector retrieval behind the same `Retriever` interface.
- Curated user-group FAQ authoring workflow.
- Stage 2C low-weight raw ops_chat retrieval, with low-confidence phrasing.
- Stage 2D automatic FAQ candidate extraction and human approval.
