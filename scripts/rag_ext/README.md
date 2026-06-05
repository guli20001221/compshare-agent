# scripts/rag_ext — external tool/ops corpus (RAG Phase 1+)

This pipeline builds **`deploy/kb/external_w0.jsonl`** — a *separate* pinned corpus of
general, platform-agnostic tool/ops knowledge (vLLM / sglang / ComfyUI / Ollama +
in-instance GPU troubleshooting). It is loaded *alongside* the platform corpus
(`deploy/kb/stage2b_w0.jsonl`) via `knowledge.LoadPinnedCorporaWithEmbeddings`, so the
platform corpus stays byte-identical.

## Why a separate corpus

- Platform knowledge is maintained elsewhere (a GitLab repo) and is OUT of scope here.
- External chunks carry `source_origin: external_official | external_community` (not
  `official`), so provenance is auditable and the runtime/eval can tell them apart.
- Its own digest pins (`ExternalCorpusDigestExpected` / `ExternalEmbeddingDigestExpectedQwen3`)
  mean rebuilding it never touches the platform pins or the frozen 377-Q platform parity.

## Authoring discipline (correctness-gated)

Curation is done by a strong model (Claude Opus) from **authoritative sources**:

1. Prefer **official tool docs** (vLLM/sglang/ComfyUI/Ollama). `source_origin: external_official`.
2. Competitor-community content may be used only after a **neutral rewrite**: strip the
   competitor platform's name/UI/console references, keep the generally-true technical
   content. `source_origin: external_community`. **Never re-attribute another platform's
   claims to CompShare.** Never fabricate flags / UI steps / field names not in the source.
3. Cite the source in `source_refs` (e.g. `vllm-docs:getting_started/quickstart`).
   `surface_url` stays `null` — external doc URLs are not on the platform surface allowlist.
4. `acl: customer_safe`, `evidence_kind: knowledge`. `product_area` is one of the external
   areas in `scripts/rag_w0/common.py` ALLOWED_PRODUCT_AREAS (`inference_serving`,
   `gpu_troubleshooting`, ...). Content ≤ 4000 runes.

## Build flow (reuses the W0 tail stages — no fork)

```
# 1. Author candidate chunks (Opus) -> scripts/rag_ext/external_candidate_w0.jsonl
python -m scripts.rag_ext.build_pilot_chunks

# 2. Schema/leakage validate (shared validator, now enforces source_origin)
python -m scripts.rag_w0.validate_chunks --chunks scripts/rag_ext/external_candidate_w0.jsonl
python -m scripts.rag_w0.check_internal_leakage --chunks scripts/rag_ext/external_candidate_w0.jsonl

# 3. Promote to deploy/kb/external_w0.jsonl, then build the qwen3 sidecar
python -m scripts.rag_w0.build_corpus_embeddings \
    --corpus deploy/kb/external_w0.jsonl --out-dir deploy/kb \
    --env F:/compshare-agent/.env.local --embed-model qwen3-embedding-8b

# 4. Pin the two external digests (corpus + qwen3 sidecar) in
#    internal/knowledge/corpus_digest.go, then:
#      - external retrieval eval (Top-3 >= 0.85)  : scripts.rag_w0.evaluate_retrieval
#      - external answer/faithfulness (judge key) : scripts.rag_w0.evaluate_answers
#      - Go loads external_w0.jsonl clean + parity (TestLoadPinnedCorpus...)
```

The judge for the answer eval uses `RAG_EVAL_JUDGE_API_KEY` (env only, never committed).
Platform non-regression (377-Q parity + Top-3) must stay unchanged — external is additive.
