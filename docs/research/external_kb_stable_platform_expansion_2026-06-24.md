# External KB Stable Platform-Adjacent Expansion (2026-06-24)

## Summary

This report records the stable external KB expansion on branch
`codex/kb-compshare-docs-main-audit`.

The external corpus remains at 255 chunks after replacing the prior
community-image-specific chunk set with stable scene-oriented material. The
31 new chunks plus 13 downgraded scene chunks are platform-neutral and
intentionally low-churn: OpenAI-compatible API semantics, RAG/Agent application
basics, AI coding agent operations, remote desktop/browser automation, data
transfer, secret handling, WebUI exposure, audio/video/CV troubleshooting,
robotics simulation, and AI4Science reproducibility.

Volatile platform facts remain out of the external corpus. Pricing, package
rules, model availability, console paths, launch announcements, and current
community-image rankings belong in the internal platform corpus and its
incremental update path.

## Added Coverage

| Group | Chunks | Examples |
|---|---:|---|
| `stable_platform_external` | 31 | OpenAI-compatible base URL/API key, streaming, retry policy, tool calling, structured output, embedding/rerank, RAG indexing, Dify/RAGFlow/n8n/Flowise/LangGraph basics, AI coding agents, MCP, remote desktop, browser automation, object storage, rsync, package proxies, secret handling, WebUI security, audio/video/CV/robotics/AI4Science runbooks |
| `professional_scene_signals` | 13 | Stable scene-oriented replacements for prior community-image-specific chunks: video generation, digital human latency, audio-driven avatar video, voice conversion, TTS selection/evaluation, LoRA training, image editing, low-VRAM video generation, quantized ComfyUI loaders, single-image 3D, and video dubbing |

## Generated Artifacts

- External corpus: `deploy/kb/external_w0.jsonl`
- External golden questions: `scripts/rag_ext/external_golden.jsonl`
- External qwen3 sidecar:
  `deploy/kb/embeddings_cc3546678c5a5c21f46f77da83f98900eaf32fba3c289372f452abbbd3b1b4a7_qwen3-embedding-8b.jsonl`
- Retrieval evaluation:
  `eval/rag_ext_external_retrieval_2026-06-24.json`

## Pins

- External corpus digest:
  `cc3546678c5a5c21f46f77da83f98900eaf32fba3c289372f452abbbd3b1b4a7`
- External qwen3 sidecar digest:
  `332d2b2ce9500a7077bfb894a6b7a303bf7a43fe6acf4abdad90545bfc8f2f8b`

These supersede the third-wave external digest
`d76f2cc633987cac4c88bcb3339ea50e262099a7eb14995e7a90b030ab909d38`, the
intermediate stable-expansion digest
`dc45a249a25b37cbd9f5d29be57c0ed6bb4617a677c177d952dbf01831670854`, and their
sidecar digests.

## Verification

- `python scripts/rag_ext/build_pilot_chunks.py` -> 255 chunks
- `python scripts/rag_ext/build_external_golden.py` -> 255 golden questions
- Direct coverage check -> 255/255 chunks covered
- Stable new/replacement chunk volatile-term check -> 0 flagged
- Old community-image-specific chunk ids in generated corpus -> 0
- `python scripts/rag_w0/validate_chunks.py --chunks deploy/kb/external_w0.jsonl` -> pass
- `python scripts/rag_w0/check_internal_leakage.py --chunks deploy/kb/external_w0.jsonl` -> 255 chunks, 0 flagged
- `python scripts/rag_ext/run_external_retrieval_eval.py ... --mode qwen3_rrf` -> Top-3 hit rate 1.0 (255/255), `stable_platform_external` 31/31, `professional_scene_signals` 13/13
