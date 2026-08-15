# RAG V2 preprocessing and release pipeline

V2 rebuilds the platform corpus from pinned public source documents and extends the external corpus from source snapshots. It does not redact or rewrite public text. Images are build-time evidence: VL converts them into structured captions, visible text, controls, and spatial relations; runtime chunks contain no hosted image or clickable image URL. Private chats, tickets, internal admin pages, temporary signed links, and unpinned inputs are rejected at source selection instead of being redacted later.

The preprocessing pipeline is reusable and independent of the runtime Agentic
RAG policy. It emits a runtime-compatible JSONL projection plus traceability
fields that older loaders may ignore. Runtime query planning, `SearchKnowledge`,
retrieval modes, embedding, reranking, citation validation, and answer synthesis
can evolve separately without changing the source-lock, build, validation, and
promotion stages described here.

## Inputs

- `compshare-docs/main` checked out at an explicit commit
- three public FAQ ZIP exports, in this order: model package, ComfyUI base image, platform usage
- three external ZIP snapshots, in this order: ComfyUI mirror, digital-human, voice/audio
- the existing external corpus, retained as an explicitly non-rebuildable legacy slice because its original source snapshot is unavailable

## Build

```powershell
python -m scripts.rag_v2.build `
  --internal-docs F:\compshare-agent-rag-v2-sources\compshare-docs `
  --internal-revision 8a81268e3d275d4767d045554680e7c5ddf82a9d `
  --faq-zip 'G:\下载\优云智算模型套餐FAQ.zip' `
  --faq-zip 'G:\下载\ComfyUI基础镜像常见问题解答（持续更新中）.zip' `
  --faq-zip 'G:\下载\优云智算使用问题FAQ.zip' `
  --external-zip 'C:\Users\23843\Documents\Codex\2026-07-15\new-chat\outputs\comfyui-mirror-docs.zip' `
  --external-zip 'C:\Users\23843\Documents\Codex\2026-07-15\new-chat\outputs\digital-human-docs.zip' `
  --external-zip 'C:\Users\23843\Documents\Codex\2026-07-15\new-chat\outputs\voice-audio-docs.zip' `
  --legacy-external deploy\kb\v2\legacy_external_lock.jsonl `
  --out-dir deploy\kb\v2 `
  --env F:\compshare-agent\.env.local `
  --valid-from 2026-07-15
```

`--skip-vl` and `--skip-semantic` are only for deterministic unit tests and local diagnosis. A release build must process all referenced images and must fail closed when a required asset cannot be extracted.

External snapshots must include every source-local image that carries workflow,
input/output, architecture, or UI evidence. The pipeline does not fetch an
unpinned current copy from an upstream repository when a pinned ZIP omitted the
file. Remote images are retried, GitHub `blob` links are normalized to raw
content, animated GIF/WebP inputs are flattened into a 4-frame contact sheet,
and decorative badges are excluded before VL. Missing external images are
recorded in `asset_report.json` and omitted from runtime text; no caption or
placeholder is fabricated.

Flattening is **not optional**: the contact sheet is what the model is shown, so
it decides caption content, and captions are cached across builds under a
contract digest. Two boundaries, deliberately different widths:

- **Pillow is required for any GIF/WebP**, because telling a static one from an
  animated one already needs a decoder.
- **The pinned Pillow version is required only when a sheet is actually
  rendered**, i.e. for a genuinely animated image. A single-frame GIF/WebP is
  handed to the model untouched, so no version can change what it sees, and
  failing that build would be failing over a rendering that never happens.

The pinned version is part of the caption contract, so changing it re-earns
every animated caption rather than silently mixing two renderings under one
digest. It also, today, re-earns every OTHER caption too — the contract is
global — which is why raising the pin is a deliberate act and not a routine
dependency bump.

```bash
python -m pip install -r scripts/rag_v2/requirements.txt
```

When a remote image cannot be revalidated and cached bytes are on hand, the
build keeps the cached bytes rather than failing over a transient error — but
it records the degrade in `asset_report.json` under `degradations`, and for
platform (non-third-party) sources that is an error the release gate blocks on
unless `--allow-stale-remote` is passed deliberately.

The output is an immutable release bundle containing both corpora, copied local assets, model caches, source locks, and a release manifest. Model caches are build artifacts and should not be committed.

The committed release keeps the promoted runtime sidecars only. Candidate
sidecars in `deploy/kb/v2` are transient because they duplicate roughly 150 MB
of runtime data.

## Cutting a release

One command covers build → refresh embeddings → diff → promote → repin →
offline bundle check:

First read the release production is serving, because the candidate has to
declare what it is built on:

```powershell
kubectl -n prj-ucompshare-prod exec compshare-kb-0 -- wget -qO- http://127.0.0.1:8088/healthz
```

```powershell
python -m scripts.rag_v2.release --env .env.local `
  --parent-release-id kb-release-<that release_id> `
  --build-arg --internal-docs --build-arg <docs-checkout> `
  --build-arg --internal-revision --build-arg <sha> `
  --build-arg --valid-from --build-arg 2026-08-16 `
  ... (one --build-arg per token; see scripts/rag_v2/build.py for the full list)
```

That id is recorded in `deploy/kb/v2/release_base.json` and committed with the
corpus. The import job refuses a candidate whose recorded parent is not what
production is currently serving, and passes it to the worker as
`--parent-release-id` so the later publish is a compare-and-swap. It cannot be
derived at import time: reading the active release then records whatever is live
when the job runs, and the two differ exactly when it matters — build on R0, let
R1 publish while the candidate is in review, and an import-time read would file
the candidate as R1's child, so publishing it overwrites content it never saw
while the base match reports success.

It does **not** publish. Publication is two separate manual GitLab jobs so that
a person reads the diff in between:

1. `import-knowledge-release` — copies the promoted corpora into a throwaway
   Pod, imports and validates a **candidate**, and leaves the active pointer
   alone. Nothing a user sees changes. It attaches `release_diff.md` and
   `asset_report.json` as job artifacts.
2. `publish-knowledge-release` — runs `--action publish` on the serving pod and
   waits for `/healthz` to report the new `kb_version`.

`release_diff.md` is written into `deploy/kb/v2` and is meant to be committed
with the corpus, because the reviewer needs it before the candidate exists. It
compares chunks by SLOT rather than by `chunk_id` (which digests the content, so
every edit would otherwise read as one deletion plus one addition), and it pairs
renamed documents — the `pages/` → `content/` migration renamed 227 documents at
once, which rendered as 232 additions beside 235 deletions before pairing and as
5 additions and 9 deletions after.

Incrementality lives entirely in the build. Captions, semantic plans and
embeddings are all content-addressed and skip unchanged work; the database
always receives a complete immutable candidate and an atomic pointer swap. Do
not make the ingest incremental — the atomic swap is what makes a release
reviewable and reversible.

Rolling back is **not** automated. A retired release keeps its chunks and
vectors, but no code path re-activates one. Emergency recovery today is
`UPDATE knowledge.kb_releases SET status='validated' WHERE id='<old-id>'`
followed by `--action publish --release-id <old-id>` — two transactions, not
one, so treat it as recovery rather than a supported rollback.

## Evaluation

Release quality is measured on 50 manually reviewed, RAG-relevant production
retrieval queries from 2026-06-26 through 2026-07-09. Raw chat text and local
judge caches stay under ignored `eval/.cache`; committed reports contain only
one-way case hashes, categories, retrieved chunk IDs, and aggregate grades.

The older 255-question synthetic set is retained only as a compatibility
regression. It must not be presented as evidence of current retrieval quality.

Fast deterministic rebuilds reuse locked VL and semantic outputs. Network link
checks, embedding refresh, real-chat reranking, and independent judging are
separate release checks and are expected to take longer than the build itself.
