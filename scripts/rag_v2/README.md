# RAG V2 preprocessing and release pipeline

V2 rebuilds the platform corpus from pinned public source documents and extends the external corpus from source snapshots. It does not redact or rewrite public text. Images are build-time evidence: VL converts them into structured captions, visible text, controls, and spatial relations; runtime chunks contain no hosted image or clickable image URL. Private chats, tickets, internal admin pages, temporary signed links, and unpinned inputs are rejected at source selection instead of being redacted later.

The current Agentic RAG planner, `SearchKnowledge`, retrieval modes, embedding model, reranker, citation validator, and answer synthesis remain unchanged. V2 emits a runtime-compatible JSONL projection plus traceability fields that older loaders may ignore.

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
content, animated GIF/WebP inputs are flattened when Pillow is available, and
decorative badges are excluded before VL. Missing external images are recorded
in `asset_report.json` and omitted from runtime text; no caption or placeholder
is fabricated.

The output is an immutable release bundle containing both corpora, copied local assets, model caches, source locks, and a release manifest. Model caches are build artifacts and should not be committed.

After validating the candidate corpora, build or incrementally refresh their
`qwen3-embedding-8b` sidecars in the release directory, then promote them to
the runtime paths:

```powershell
python -m scripts.rag_v2.promote --release-dir deploy\kb\v2 --deploy-dir deploy\kb
```

The committed release keeps the promoted runtime sidecars only. Candidate
sidecars in `deploy/kb/v2` are transient because they duplicate roughly 150 MB
of runtime data.

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
