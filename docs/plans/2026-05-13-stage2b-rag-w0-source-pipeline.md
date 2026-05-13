# Stage 2B RAG W0 Source Pipeline Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build a safe, traceable W0 RAG source-processing pipeline before importing the full 优云智算 / ModelVerse knowledge base.

**Architecture:** Process every source into a governed intermediate corpus before chunking. Markdown, PDF, images, and external links are parsed together, converted into normalized Markdown with asset notes, cleaned for customer safety, then chunked and evaluated. Runtime RAG only consumes approved chunks; internal case records never enter customer-facing retrieval directly.

**Tech Stack:** Python offline source pipeline, Go `internal/knowledge` runtime schema/retriever, JSONL corpora, Markdown normalized documents, asset manifests, ModelVerse/Qwen VL or equivalent VL for image descriptions, ds v4 pro for batch rewriting, Claude Opus 4.7 or equivalent strong judge for sampled evaluation.

---

## 0. Current Baseline

### 0.1 Code Baseline

Current product code is already past the Pre-RAG gate:

- PR #64 read-only mutation boundary
- PR #65 finance routing + FAQ split
- PR #66 trace closure
- PR #67 monitor UX / FAQ fix

The latest reviewed main is `6afc139`.

### 0.2 Source Bundle

All currently known sources are staged locally under:

`F:\compshare-agent-runs\rag-source-bundle-20260512`

Manifest:

`F:\compshare-agent-runs\rag-source-bundle-20260512\manifest.json`

Current source classes:

| Source | Path / status | Decision |
| --- | --- | --- |
| GitLab `compshare-docs` | `gitlab-compshare-docs/` | Include by directory policy, not blanket import |
| Feishu model package FAQ | `feishu/feishu-model-package-faq-latest.md` | Include after cleaning |
| Feishu usage FAQ | `feishu/feishu-usage-faq-latest.md` | Include after cleaning |
| Legacy Feishu exports | `feishu/*legacy*.md` | Reference only unless latest files are incomplete |
| Windows remote login runbook | `runbooks/zips/*Windows服务器远程登录*` | Include after PDF/image cleaning |
| NVIDIA driver / CUDA runbook | `runbooks/zips/*NVIDIA驱动安装和CUDA安装*` | Include after PDF/image cleaning |
| Remote Windows sound runbook | `runbooks/zips/*远程windows云服务器开启声音*` | Include after PDF/image cleaning |
| Internal case QA zip | `runbooks/zips/*算力case处理qa文档*` | Internal reference only; split into customer-safe notes first |
| Enterprise WeChat SPT export | `internal_cases/spt-record.txt` | Internal case mining only; never raw customer-facing RAG |

`spt-record.txt` has been copied into:

`F:\compshare-agent-runs\rag-source-bundle-20260512\internal_cases\spt-record.txt`

The manifest entry must remain:

```json
{
  "id": "wxwork-spt-record-2026-05",
  "type": "internal_case_chat_export",
  "include_status": "internal_reference_only_needs_customer_safe_split",
  "customer_safe": "false"
}
```

## 1. Product Scope

### 1.1 In Scope For W0

W0 should support four product surfaces:

1. **智能会话**
   - billing rules, refund rules, invoice how-to
   - remote login, Jupyter, SSH, Windows RDP
   - image types, GPU specs, ModelVerse package / credit basics
   - common errors and usage guidance

2. **操作引导**
   - start / stop / reboot / reset password guidance only
   - cannot connect / CPU high / init failure / driver install guidance
   - no actual mutating action execution
   - no SSH login, remote command execution, or instance-internal file read

3. **资源查询**
   - still API-first for live facts: instance list, image list, specs, inventory, price
   - RAG may explain concepts but must not replace live API facts

4. **当前监控分析**
   - current monitor only
   - historical trend, charts, and arbitrary time-window monitor remain next stage

### 1.2 Out Of Scope For W0

- Full-document ingestion without source governance
- User-facing answers from raw Enterprise WeChat chats
- Internal staff names, work orders, internal URLs, passwords, tokens, or non-standard backend operations
- Historical monitor trend charts
- Remote instance commands
- Account real-time finance data: balance, invoice status, refund progress, monthly bill, transaction flow

## 2. Non-Negotiable Design Rules

### 2.1 Assets Before Chunks

Images, PDFs, and external links are part of the source document. They must be processed before chunking.

Correct order:

```text
raw source bundle
→ source manifest
→ asset extraction
→ PDF text/image extraction
→ image OCR/VL descriptions
→ external link classification/snapshot
→ normalized Markdown with asset notes in semantic position
→ cleaning / customer-safety rewrite
→ chunking
→ eval
→ runtime corpus
```

Do not cut text chunks first and then patch image notes later.

### 2.2 Images Need Instruction-Aware Notes

Plain captions are insufficient for operation screenshots.

Bad:

```markdown
[图片说明]
该截图展示 Windows 远程登录页面，用户需要填写公网 IP、用户名和密码。
```

Required for instructional images:

```markdown
[操作截图说明]
场景：Windows 远程桌面连接
用户目标：填写远程连接地址
图中重点：红框标出了“计算机”输入框
应填写：实例公网 IP
不要填写：实例名称、内网 IP、控制台登录账号
下一步：点击“连接”
注意：如果连接失败，应继续检查安全组、端口、实例状态和远程桌面服务
不确定：截图未展示安全组页面，因此不能判断安全组是否已放行
```

Image note fields:

- `asset_id`
- `source_doc_id`
- `heading_path`
- `image_path`
- `nearby_text`
- `visual_type`: `operation_screenshot | error_screenshot | console_state | diagram | logo | qr_code | decorative | unknown`
- `description`
- `highlighted_ui`
- `user_action`
- `expected_input`
- `next_step`
- `caveats`
- `confidence`
- `include_in_rag`: `true | false`
- `exclusion_reason`

PR-RAG-2 deliberately emits deterministic image notes only. It records the full
image-note schema and sets `model_metadata.vl_executed=false` / `vl_status` so
the later OCR/VL pass can replace or approve the note before chunk promotion.
This keeps W0 source governance moving without claiming visual facts that were
not actually read by a VL model.

Rules:

- every image reference must receive one final state before chunking:
  - `included_with_vl_note`
  - `included_with_ocr_note`
  - `excluded_low_value`
  - `excluded_qr_or_group`
  - `missing_asset`
  - `external_asset_snapshot_required`
  - `external_asset_snapshot_failed`
  - `duplicate_asset`
- image hashes must be recorded so duplicate screenshots are not repeatedly sent to VL;
- `operation_screenshot`, `error_screenshot`, and meaningful `console_state` can enter normalized docs.
- `logo`, `qr_code`, `decorative`, and low-information screenshots are excluded from RAG but kept in asset manifest.
- QR codes and group invite images must never enter customer-facing answers.

### 2.3 External Links Are Rule-Gated First

LLM may help judge link usefulness, but it must not be the only decision-maker.

First-pass deterministic classes:

| Link type | Default decision |
| --- | --- |
| Official console route | Keep as navigation hint; do not treat as factual text |
| Internal docs already in source bundle | Resolve locally if possible |
| GitLab / Feishu source links | Keep as provenance; do not expose internal URLs to users |
| API docs | Keep, usually lower weight unless user asks API usage |
| Public official docs | Snapshot only if needed for W0 answer |
| Temporary download link | Exclude from answer |
| Activity / marketing page | Exclude unless explicitly in product scope |
| QR code / group link | Exclude |
| Work order / demand / admin URL | Internal provenance only; never customer-facing |
| Unknown external URL | Record only; require review before inclusion |

LLM may answer:

- Is the linked content necessary to understand the current step?
- Does the surrounding text already contain enough information?
- Should the link become a citation, navigation hint, or excluded asset?

Final decision is rule + allowlist + LLM note, not LLM alone.

Any link used as answer evidence must have one of these final states before chunking:

- `snapshotted`: local snapshot path + SHA-256 hash are recorded;
- `local_source_resolved`: resolved to another source bundle file;
- `navigation_only`: allowed only as a control-console direction, not a factual source;
- `excluded`: explicit reason recorded;
- `review_required`: blocked from chunking until reviewed.

Chunks must not cite `review_required`, `unknown`, failed snapshot, admin/work-order, QR/group, or temporary download links.

### 2.4 Internal Case Records Are Not Customer Knowledge

`spt-record.txt` and internal case QA docs are valuable, but raw text must not be loaded into customer-facing RAG.

Allowed uses:

- case mining
- real user question generation
- failure mode taxonomy
- customer-safe FAQ/runbook rewrite drafts
- eval set generation

Forbidden uses:

- raw chat as retrieval chunk
- staff names in answers
- customer IDs/resource IDs in answers
- passwords/token-like strings in corpus
- internal work order links in answers
- non-standard backend process instructions in answers
- promises like “数据一定没事”

Internal case promotion requires a separate approval record.

Each candidate derived from `spt-record.txt` or internal case QA must move through:

```text
raw_internal_case
→ redacted_case_note
→ customer_safe_rewrite_candidate
→ deterministic safety scan
→ LLM judge review
→ human/owner approval
→ approved_faq_candidate
→ runtime faq chunk
```

Approval fields:

- `case_id`
- `source_hash`
- `redaction_status`
- `rewrite_path`
- `approved_by`
- `approved_at`
- `allowed_product_area`
- `blocked_phrases_checked`
- `final_runtime_chunk_id`

Default state is not approved. Unapproved case rewrites may be used for eval questions, but not for deployed RAG chunks.

### 2.5 Runtime Corpus Must Match Current Loader

Current runtime loader accepts only:

- `source_type=faq`
- `source_type=runbook`

Therefore W0 deployed chunks must use only these two source types until a later runtime-schema PR expands `internal/knowledge/loader.go`.

Do not put these experimental types directly into deployed `deploy/kb/*.jsonl`:

- `asset_note`
- `case_rewrite`
- `doc`
- `api_reference`
- `internal_case`

Instead:

- image/PDF notes become supporting text inside an approved `runbook` or `faq` chunk;
- internal case rewrites become approved `faq` chunks only after separate approval;
- original source class is preserved in `source_refs` / sidecar manifests, not in runtime `source_type`.

If a future PR needs richer source types, that PR must include loader validation changes and tests before any corpus uses the new values.

### 2.6 Retrieval Trace Schema v1

When PR-RAG-6 wires retrieval into the runtime, every turn that triggers `knowledge_qa` retrieval emits a `RetrievalTrace` record into the existing `trace.v0.2` observability stream.

The v1 field set is intentionally minimal and fixed:

```json
{
  "enabled": true,
  "kb_version": "kb.stage2b.w0.2026-05-13",
  "query_normalized": "...",
  "hits": 2,
  "hit_items": [
    { "chunk_id": "w0-login-windows-rdp-001", "score": 0.78, "kept": true },
    { "chunk_id": "w0-login-jupyter-002", "score": 0.41, "kept": false }
  ]
}
```

Semantics:

- `enabled`: `false` when retrieval was skipped (e.g., non-`knowledge_qa` intent), `true` otherwise.
- `kb_version`: corpus version actually used at retrieval time.
- `query_normalized`: normalized user query used for retrieval; for cross-turn audit.
- `hits`: integer count of returned candidates. This field already exists in `trace.v0.2`; keep its type unchanged.
- `hit_items[]`: top-K candidates that passed offline safety filtering and were considered at retrieval time.
- `hit_items[].kept`: among those candidates, whether the chunk was finally used in the answer. Internal / unapproved / `acl != customer_safe` chunks NEVER reach `hit_items[]` — they are filtered offline and cannot appear with `kept=false`. Only "passed safety, was retrieved, then dropped by ranking / diversification / top-K" candidates can be `kept=false`.

Explicitly NOT in v1 (must wait for a follow-up PR):

- cross-chunk relevance / diversification metadata
- retrieval planning chains
- shadow-vs-prod comparison fields
- per-hit highlight spans
- per-hit token cost

Dependencies:

- PR-RAG-4 must reference these field names when writing the `expected_behavior` enum and expected chunk assertions in `eval/rag/w0/golden_questions.jsonl`.
- PR-RAG-5 must compute retrieval metrics directly from this trace record.
- PR-RAG-6 must populate the field on every `knowledge_qa` turn (and explicitly emit `enabled=false` for other intents).

Where in the trace structure: extend the existing `Retrieval` field on `TraceRecord` in `internal/observability/trace.go`. Existing `trace.v0.2` schema version stays because `hits` remains an integer count and `hit_items` is additive and optional in JSON.

### 2.7 Eval Expected Behavior Enum

W0 eval records must use a fixed `expected_behavior` enum:

- `answer`: should answer from retrieved evidence, optionally with allowed `surface_url` citations.
- `refuse`: should refuse the requested operation or claim, while still giving safe guidance.
- `hard_block`: should return a fixed hard-block response without asking the LLM to improvise (for example, account real-time finance).
- `escalate`: should suggest contacting platform support or filing a ticket.

Adding another value requires a plan PR and synchronized updates to `evaluate_retrieval.py`, `evaluate_answers.py`, and the golden question schema.

## 3. W0 Corpus Target

W0 is intentionally small. Target 40-100 high-quality chunks.

Priority topics:

| Product area | Must cover |
| --- | --- |
| login | SSH, Jupyter, Windows RDP, password reset guidance |
| monitor | current CPU/memory/GPU/VRAM meaning; historical unsupported wording |
| billing_rule | shutdown billing, prepaid/postpaid/dynamic, invoice how-to, refund rules, arrears handling |
| image | platform/community/private/base image concepts |
| driver_cuda | NVIDIA driver and CUDA install guidance |
| windows | Windows remote login and remote sound guidance |
| modelverse | package/credit/common usage FAQ |
| init_failure | common init failure and cannot-connect guidance |
| resource_purchase | GPU spec/inventory/purchase consultation basics |

W0 must not aim for full GitLab coverage.

## 4. Planned Output Layout

All generated artifacts should go under a new local run directory first:

`F:\compshare-agent-runs\rag-w0-YYYYMMDD-HHMMSS`

Expected layout:

```text
rag-w0-YYYYMMDD-HHMMSS/
  source_manifest.json
  asset_manifest.json
  link_manifest.json
  normalized_docs/
  cleaned_docs/
  internal_case_mining/
  chunks/
    chunks_w0.jsonl
  eval/
    golden_questions_w0.jsonl
    retrieval_eval.json
    answer_eval.json
    safety_eval.json
    eval_report.md
  logs/
```

Only approved, compact outputs should later be copied into the repo:

```text
deploy/kb/stage2b_w0.jsonl
eval/rag/w0/golden_questions.jsonl
eval/rag/w0/eval_report.md
docs/plans/2026-05-13-stage2b-rag-w0-source-pipeline.md
```

Do not commit the raw source bundle wholesale.

## 5. Proposed Code/Script Additions

Create a separate offline script package:

```text
scripts/rag_w0/
  build_source_manifest.py
  extract_assets.py
  classify_links.py
  describe_images.py
  normalize_docs.py
  clean_docs.py
  mine_internal_cases.py
  chunk_docs.py
  generate_eval_questions.py
  evaluate_retrieval.py
  evaluate_answers.py
  README.md
```

Runtime code changes should be delayed until W0 corpus quality passes.

Possible later runtime files:

```text
internal/knowledge/chunk.go
internal/knowledge/loader.go
internal/knowledge/retriever.go
internal/knowledge/renderer.go
internal/observability/trace.go
```

## 6. Model Usage Policy

### 6.1 Deterministic First

Use deterministic code for:

- file discovery
- zip extraction
- Markdown parsing
- PDF text extraction
- image reference extraction
- link extraction
- hash generation
- duplicate detection
- simple rule-based exclusion
- schema validation

### 6.2 LLM-Assisted Processing

LLM may be used for:

- operation screenshot descriptions
- OCR cleanup
- PDF screenshot interpretation
- internal case summarization
- customer-safe rewrite
- chunk quality grading
- eval question generation

Recommended split:

- ds v4 pro: batch cleaning, FAQ rewriting, image/PDF note drafting
- Qwen VL or equivalent: image descriptions, especially Chinese console screenshots
- Claude Opus 4.7 or equivalent strong judge: sampled safety/grounding review and difficult case adjudication

Judge must not be the only gate. Use rule checks + LLM judge + human spot check.

### 6.3 LLM Output Must Be Auditable

Every LLM-produced note must store:

- model
- prompt version
- input source ID
- output hash
- confidence
- review status
- whether it is allowed into customer-facing corpus

## 7. Evaluation Gates

### 7.1 Source Coverage Gate

Fail if:

- any manifest source path is missing
- PDF/ZIP extraction fails silently
- image references are not recorded
- links are not classified
- generated files contain GitLab/Feishu credentials
- raw Enterprise WeChat content is present in customer-facing chunks

Metrics:

- document count
- PDF parse success rate
- image reference count
- image included/excluded count
- link classified count
- duplicate document count
- invalid encoding / mojibake count

### 7.2 Cleaning Gate

Fail if customer-facing cleaned docs contain:

- staff names
- customer IDs
- raw `uhost-*`, `uimage-*`, `bsi-*` from internal cases
- passwords or password-like strings
- token/private key patterns
- internal admin URLs
- “找某某处理” style internal routing
- non-standard backend operation steps
- guarantees about customer data integrity

### 7.3 Chunk Quality Gate

Each chunk must satisfy:

- one main topic
- self-contained enough for retrieval answer
- source ID and title present
- product area present
- customer safety label present
- asset references present when image/PDF content contributed facts
- no raw internal-only content

Chunk fields:

```json
{
  "chunk_id": "w0-login-windows-rdp-001",
  "kb_version": "kb.stage2b.w0.2026-05-13",
  "source_type": "faq|runbook",
  "product_area": "login|billing_rule|monitor|image|driver_cuda|windows|modelverse|init_failure|resource_purchase",
  "acl": "customer_safe",
  "title": "...",
  "question_patterns": ["..."],
  "content": "...",
  "source_refs": ["..."],
  "asset_refs": ["..."],
  "confidence": "high|medium|low",
  "valid_from": "2026-05-13",
  "evidence_kind": "knowledge",
  "surface_url": null,
  "retrieval_score_hint": null
}
```

Sidecar manifests may contain richer source classes, but deployed runtime chunks must stay compatible with the current loader.

**Reserved fields for the runtime Evidence type.**

The last three fields (`evidence_kind`, `surface_url`, `retrieval_score_hint`) are forward-compatible placeholders that PR-RAG-6 (runtime hookup) will consume when constructing the runtime Evidence value. The current loader ignores them, but chunks must include them so the W0 corpus does not need re-emission after Evidence lands.

- `evidence_kind`: enum. W0 values fixed at `"knowledge"`. Future values reserved: `"api_fact"`, `"diagnosis"`, `"workflow_result"`. Any other value must be added in a follow-up loader PR.
- `surface_url`: nullable. When non-null, must be a customer-facing URL — see policy below.
- `retrieval_score_hint`: nullable. Optional offline-computed retrieval prior. Runtime score wins; this is a hint only.

**`surface_url` host policy (non-negotiable).**

`surface_url` is what gets rendered to the end user as a citation. It must be safe to expose.

Allowed hosts (initial W0 allowlist, anchored to existing corpus only):

```text
console.compshare.cn          # console navigation hint
www.compshare.cn              # only paths under /docs/
```

Pending hosts (must be team-confirmed before adding to the allowlist):

- any other compshare-owned customer-facing domain (no other `docs.*` / `help.*` host has been confirmed yet)
- any third-party host (community forum, vendor docs) — case-by-case approval

Until a host is in the allowed list, validators must fail any `surface_url` referencing it.

Denied (validators must fail these regardless of host):

- any `*.gitlab.*` host (internal code hosting)
- any `*.feishu.cn` / `*.lark.com` / `*.feishu.*` host (internal collaboration)
- any URL whose path contains `/admin`, `/workorder`, or `/internal`
- any URL whose query contains `token=`, `signature=`, or other signed-URL parameters
- any URL whose scheme is not `https`
- any temporary download link (host or query indicates expiration)

A null `surface_url` is allowed and is the safer default — only set a value when the source URL is verified-safe to render in a customer-facing answer. Internal source provenance (GitLab path, Feishu doc id, internal case id) MUST NOT appear in `surface_url`. Use `source_refs` for internal provenance — that field is for audit/trace only and never reaches the user.

### 7.4 Retrieval Gate

Create at least 50 W0 golden questions.

Required groups:

- remote login / SSH / Jupyter
- Windows RDP / Windows sound
- CUDA / NVIDIA driver
- CPU high / init failure
- shutdown billing / billing mode difference
- invoice how-to / refund rules / arrears
- ModelVerse package / credit
- image types / GPU specs
- should hard-block account real-time finance
- should refuse unsupported instance-internal operation

Metrics:

- Top-1 hit rate
- Top-3 hit rate
- wrong-area retrieval rate
- no-answer correctness
- internal leakage count

Minimum W0 target:

- Top-3 hit rate >= 85% on curated W0 questions
- internal leakage = 0
- hard-block / no-answer correctness = 100% for safety cases

### 7.5 Answer Quality Gate

For generated answers, judge:

- uses only retrieved chunks and allowed API facts
- no invented amounts, statuses, timestamps, or resource facts
- no internal links/staff/process leakage
- gives useful next step
- cites or references source title where possible
- says “无法确认” when retrieved evidence is insufficient

Use Claude Opus 4.7 or equivalent as sampled judge after deterministic checks.

### 7.6 Reproducible Validation Commands

Every PR that produces W0 artifacts must add or update fixed validation commands.

Required commands by stage:

```powershell
python scripts/rag_w0/build_source_manifest.py --bundle F:\compshare-agent-runs\rag-source-bundle-20260512 --out F:\compshare-agent-runs\rag-w0-current\source_manifest.json
python scripts/rag_w0/validate_source.py --manifest F:\compshare-agent-runs\rag-w0-current\source_manifest.json
python scripts/rag_w0/validate_assets.py --assets F:\compshare-agent-runs\rag-w0-current\asset_manifest.json --links F:\compshare-agent-runs\rag-w0-current\link_manifest.json
python scripts/rag_w0/validate_cleaned_docs.py --dir F:\compshare-agent-runs\rag-w0-current\cleaned_docs
python scripts/rag_w0/validate_chunks.py --chunks F:\compshare-agent-runs\rag-w0-current\chunks\chunks_w0.candidate.jsonl
python scripts/rag_w0/evaluate_retrieval.py --chunks F:\compshare-agent-runs\rag-w0-current\chunks\chunks_w0.candidate.jsonl --questions eval/rag/w0/golden_questions.jsonl --out F:\compshare-agent-runs\rag-w0-current\eval\retrieval_eval.json
python scripts/rag_w0/evaluate_answers.py --chunks F:\compshare-agent-runs\rag-w0-current\chunks\chunks_w0.candidate.jsonl --questions eval/rag/w0/golden_questions.jsonl --out F:\compshare-agent-runs\rag-w0-current\eval\answer_eval.json
```

Failure rules:

- any missing source path: fail;
- any evidence link with `review_required` / missing snapshot / failed snapshot: fail;
- any image without final state: fail;
- any customer chunk with source type outside `faq|runbook`: fail;
- any customer chunk with `acl != customer_safe`: fail;
- any internal pattern leak: fail;
- any customer chunk with `evidence_kind` outside the W0 enum (W0 allows `knowledge` only): fail;
- any customer chunk whose non-null `surface_url` host is not in the W0 allowlist (`console.compshare.cn` or `www.compshare.cn` under `/docs/`): fail;
- any customer chunk whose non-null `surface_url` violates the deny rules (non-`https` scheme, internal path `/admin|/workorder|/internal`, signed-URL query parameters, or temporary-download host): fail;
- any approved case rewrite missing approval record: fail;
- retrieval Top-3 below threshold: fail;
- safety leakage above zero: fail.

## 8. Implementation Tasks

### Task 1: Source Bundle Lock And Manifest Validation

**Files:**

- Create: `scripts/rag_w0/build_source_manifest.py`
- Create: `scripts/rag_w0/README.md`
- Output only: `F:\compshare-agent-runs\rag-w0-<timestamp>\source_manifest.json`

**Steps:**

1. Read `F:\compshare-agent-runs\rag-source-bundle-20260512\manifest.json`.
2. Validate all paths exist.
3. Validate `wxwork-spt-record-2026-05` exists and is marked `customer_safe=false`.
4. Compute file counts and hashes.
5. Emit `source_manifest.json`.
6. Test with missing path fixture.

Acceptance:

- Missing source path fails loudly.
- Raw source bundle is not copied into repo.
- `spt-record.txt` is included only as internal reference.

### Task 2: Asset Extraction Before Chunking

**Files:**

- Create: `scripts/rag_w0/extract_assets.py`
- Create: `scripts/rag_w0/validate_assets.py`
- Output only: `asset_manifest.json`, `link_manifest.json`

**Steps:**

1. Parse Markdown image references.
2. Parse PDF/ZIP extracted images and embedded media.
3. Parse external links.
4. Attach each asset to source doc + heading path + nearby text.
5. Classify obvious excluded assets: QR code names, logo names, decorative paths.
6. Hash local images and mark duplicates.
7. Emit manifests.

Acceptance:

- Every image reference has a final state: included, excluded, missing, external snapshot required/failed, or duplicate.
- Every link is classified at least as `unknown`, never silently ignored.
- PDF images are represented before chunking.
- No unresolved image can silently disappear from normalized docs.

### Task 3: Instructional Image Understanding

**Files:**

- Create: `scripts/rag_w0/describe_images.py`
- Output only: `asset_notes.jsonl`

**Steps:**

1. Start from all images in `asset_manifest.json`.
2. Exclude low-value images by deterministic rule where possible.
3. Select included W0 operation/error/console screenshots for OCR/VL follow-up.
4. For PR-RAG-2, emit deterministic nearby-text notes with `vl_executed=false`.
5. Produce structured operation-note drafts, not generic captions.
6. Mark every image with a final inclusion/exclusion state.
7. Keep model metadata and confidence.

Acceptance:

- Operation screenshot drafts include user action, highlighted UI, expected input, and caveats when nearby text supports them.
- QR codes and decorative images are excluded.
- No image note claims information not visible in nearby text until the OCR/VL follow-up runs.
- All images are accounted for; only a subset needs expensive VL.

### Task 4: Link Classification And Snapshot Policy

**Files:**

- Create: `scripts/rag_w0/classify_links.py`
- Create: `scripts/rag_w0/snapshot_links.py`
- Output only: updated `link_manifest.json`

**Steps:**

1. Apply rule-based link classes.
2. Identify internal admin/work-order links.
3. Identify official docs / console navigation links.
4. Use LLM only for ambiguous “needed or not” classification.
5. Record snapshot requirement for any required external content.
6. Snapshot required external evidence links locally.
7. Record snapshot path, status, hash, and failure reason.

Acceptance:

- Internal admin links never become customer-facing citations.
- Console links are navigation hints, not factual evidence.
- Unknown links require review before inclusion.
- Any link used as answer evidence has a local snapshot or is resolved to a local source.
- Failed or unreviewed snapshots block chunking for that source section.

### Task 5: Normalize Documents With Asset Notes

**Files:**

- Create: `scripts/rag_w0/normalize_docs.py`
- Output only: `normalized_docs/*.md`

**Steps:**

1. Convert each source into normalized Markdown.
2. Insert image/PDF asset notes at semantic positions.
3. Preserve heading structure.
4. Add front matter with source ID and safety state.

Acceptance:

- Asset notes appear before chunking.
- Normalized docs are readable without opening original images/PDFs.
- Excluded assets remain referenced in manifests, not normalized content.

### Task 6: Clean Customer-Facing Content

**Files:**

- Create: `scripts/rag_w0/clean_docs.py`
- Output only: `cleaned_docs/*.md`

**Steps:**

1. Remove or rewrite internal-only content.
2. Remove staff/customer/resource identifiers.
3. Remove passwords and token-like strings.
4. Rewrite operation guidance into customer-safe wording.
5. Mark low-confidence sections for review.

Acceptance:

- Customer-facing docs pass deterministic secret/internal scan.
- Internal case sources do not produce direct customer docs.
- Each rewrite keeps source traceability.

### Task 7: Mine Enterprise WeChat Cases Safely

**Files:**

- Create: `scripts/rag_w0/mine_internal_cases.py`
- Create: `scripts/rag_w0/validate_case_approvals.py`
- Output only: `internal_case_mining/*.jsonl`

**Steps:**

1. Parse `spt-record.txt` into time/topic blocks.
2. Redact names, IDs, passwords, URLs.
3. Extract issue pattern, symptoms, likely cause, resolution, and user-safe answer candidate.
4. Label each case as `eval_only`, `faq_candidate`, or `internal_only`.
5. Reject any block that requires instance-internal operation as customer-facing answer.
6. Emit approval records separately from candidate notes.

Acceptance:

- Raw chat lines are never emitted into customer-facing corpus.
- Extracted cases are useful for eval and FAQ drafts.
- Password-like strings are absent from outputs.
- No case-derived FAQ can enter chunks without an approval record.
- Approval record must reference the redacted case hash, not raw chat text.

### Task 8: Chunk W0 Corpus

**Files:**

- Create: `scripts/rag_w0/chunk_docs.py`
- Output first: `F:\compshare-agent-runs\rag-w0-current\chunks\chunks_w0.candidate.jsonl`
- Output later after eval: `deploy/kb/stage2b_w0.jsonl`

**Steps:**

1. Chunk only approved cleaned docs and approved case rewrites.
2. Keep one topic per chunk.
3. Attach source refs and asset refs.
4. Validate schema.
5. Keep candidate chunks outside `deploy/kb` until eval passes.

Acceptance:

- 40-100 high-quality W0 chunks.
- Zero `internal_only` chunks in deployed corpus.
- No source with `customer_safe=false` enters directly.
- Candidate chunks use only runtime-compatible `source_type=faq|runbook`.
- `deploy/kb/stage2b_w0.jsonl` is created only after retrieval/answer eval passes.

### Task 9: Build W0 Eval Set

**Files:**

- Create: `scripts/rag_w0/generate_eval_questions.py`
- Output: `eval/rag/w0/golden_questions.jsonl`

**Steps:**

1. Generate and manually review at least 50 questions.
2. Include positive retrieval, refusal, and hard-block cases.
3. Include questions mined from `spt-record.txt`, rewritten safely.
4. Label expected chunks and expected behavior using the §2.7 enum.

Acceptance:

- Eval includes both natural customer wording and precise FAQ wording.
- Account real-time finance questions are expected to hard-block.
- Unsupported instance-internal actions are expected to refuse or guide.

### Task 10: Evaluate Retrieval And Answering

**Files:**

- Create: `scripts/rag_w0/evaluate_retrieval.py`
- Create: `scripts/rag_w0/evaluate_answers.py`
- Output: `eval/rag/w0/eval_report.md`

**Steps:**

1. Run deterministic retrieval eval.
2. Generate answers against retrieved chunks.
3. Run deterministic safety checks.
4. Run sampled LLM judge with Claude Opus 4.7 or equivalent.
5. Produce report.

Acceptance:

- Top-3 retrieval >= 85% for W0.
- Safety failures = 0.
- Internal leakage = 0.
- Judge report lists all questionable chunks before runtime integration.

### Task 11: Runtime Hook Only After W0 Passes

**Files:**

- Modify later: `internal/knowledge/*`
- Modify later: `cmd/trace.go`
- Modify later: `cmd/agent.go`

**Steps:**

1. Load `deploy/kb/stage2b_w0.jsonl`.
2. Keep `knowledge_qa` only.
3. Do not route live resource facts through RAG.
4. Add trace hit IDs and source refs.
5. Run CLI smoke with ds v4 flash.

Acceptance:

- Resource and current monitor still use API.
- Account real-time finance remains hard-block.
- Knowledge answers cite approved chunks.
- No raw internal sources appear in responses or trace.

## 9. PR Plan

Recommended PR sequence:

1. **PR-RAG-0:** plan + source manifest update
2. **PR-RAG-1:** offline source/asset/link manifest scripts + validators
3. **PR-RAG-2:** image/PDF/link normalization for W0 runbooks, including link snapshots
4. **PR-RAG-3:** cleaning + internal case mining + case approval records
5. **PR-RAG-4:** candidate W0 chunks + eval set; keep chunks outside `deploy/kb`
6. **PR-RAG-5:** retrieval/answer eval report; only then promote passing chunks into `deploy/kb/stage2b_w0.jsonl`
7. **PR-RAG-6:** runtime `knowledge_qa` hookup after W0 passes

Do not implement runtime RAG before PR-RAG-5 proves corpus quality.

## 10. Final Readiness Definition

This stage is ready to move from source processing to runtime RAG only when:

- source manifest validates all source paths
- every image/link/PDF asset is classified
- instructional images have structured notes or are explicitly excluded
- internal case sources are mined but not raw-loaded
- W0 chunks pass schema and safety checks
- W0 eval report passes minimum retrieval and safety gates
- a human-readable report explains what is included, excluded, and why
