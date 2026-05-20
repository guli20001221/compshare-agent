# Knowledge Base (Stage 2B W0+)

This directory holds the deployed knowledge corpus + retrieval embedding
sidecars for the CompShare console agent's RAG path.

## Files

| File | Purpose | Bound to |
|---|---|---|
| `stage2b_w0.jsonl` | Customer-safe FAQ corpus (228 chunks @ 2026-05-20, kb_version `kb.stage2b.w0.2026-05-19.package-policy`) | `internal/knowledge/corpus_digest.go:CorpusDigestExpected` |
| `embeddings_<corpus-digest>.jsonl` | `text-embedding-3-large` (3072-dim) sidecar for `hybrid_cosine` / `hybrid_rerank` modes | `internal/knowledge/corpus_digest.go:EmbeddingDigestExpected` |
| `embeddings_<corpus-digest>_qwen3-embedding-8b.jsonl` | `qwen3-embedding-8b` (4096-dim) sidecar for `qwen3_full` / `qwen3_rrf` modes | `internal/knowledge/corpus_digest.go:EmbeddingDigestExpectedQwen3` |

All three artifacts are byte-pinned by LF-normalized SHA256 digest. The Go
loader refuses to start if any pin mismatches its on-disk content.

Content policy: every chunk must be `acl="customer_safe"` and contain no
account-specific values, keys, tokens, IPs, or raw transcripts.

## Retrieval modes

Selected at runtime via `RAG_RETRIEVAL_MODE` env var (precedence over legacy
`RAG_HYBRID_ENABLED`). Parsed in `cmd/trace.go:ragRetrievalModeFromEnv`.

| Mode | Embed dependency | Reranker | Use case |
|---|---|---|---|
| `bm25_only` | None | None | Safest default; ships when no sidecar is present |
| `hybrid_cosine` | text-embedding-3-large sidecar | None | BM25 top-20 → cosine rerank |
| `hybrid_rerank` | text-embedding-3-large sidecar | bge-reranker-v2-m3 | hybrid_cosine + cross-encoder rerank |
| `qwen3_full` | qwen3-embedding-8b sidecar | qwen3-reranker-8b | Same-family qwen3 embed + rerank (Lane B) |
| `qwen3_rrf` | qwen3-embedding-8b sidecar | qwen3-reranker-8b | BM25 top-50 ⊕ dense-full-corpus top-50 fused via Reciprocal Rank Fusion (k=60) + reranker. Experimental; recovers BM25-zero-hit queries |

### Opt-in `qwen3_full` for ModelVerse-hosted deployments

`qwen3_full` is the **recommended mode for demos targeting the post-#113
corpus** based on the post-#53 cumulative-eval verdict (delta v2 2026-05-20):
net +2 strict-yes / -1 wc / -1 fab=severe vs `hybrid_cosine` on 115-Q common
set, with the cc02 disambig regression closed by PR #114. It remains opt-in
(does not become the runtime default) until a cleaner +5pp acceptance bar
is met.

To opt in at deploy time:

```bash
export RAG_RETRIEVAL_MODE=qwen3_full
# Optional explicit overrides (rarely needed; defaults are derived from mode):
# export MODELVERSE_EMBED_MODEL=qwen3-embedding-8b
# export MODELVERSE_RERANKER_MODEL=qwen3-reranker-8b
```

Both the qwen3 embedding + reranker calls go through the ModelVerse OpenAI-
compat endpoint (no separate provider). The qwen3 sidecar file must exist
on disk under `deploy/kb/` and its LF-normalized SHA256 must match
`EmbeddingDigestExpectedQwen3` — otherwise the loader refuses to start.

Operational notes:
- Empty `RAG_RETRIEVAL_MODE` falls back to legacy `RAG_HYBRID_ENABLED` check.
- Setting a non-recognized mode logs a warning and falls back the same way.
- `RAG_HYBRID_TIMEOUT_MS` / `RAG_RERANKER_TIMEOUT_MS` knob the per-call
  timeout for the embed + reranker stages respectively (defaults are
  conservative; see `internal/knowledge/retriever.go`).

### Experimental: `qwen3_rrf` (Reciprocal Rank Fusion)

`qwen3_rrf` is an alternative to `qwen3_full` that uses rank-level fusion
instead of the cascade filter. Pipeline:

  BM25 top-50  ⊕  dense-full-corpus top-50   →  RRF (k=60)  →  top-10
                                              →  qwen3-reranker-8b  →  top-3

Critical difference vs `qwen3_full`: when BM25 returns zero candidates,
`qwen3_full` skips the embedder entirely (the cascade has nothing to
rerank). `qwen3_rrf` always runs the dense leg, so vocabulary-mismatch
queries like "GPU 跑死了" (corpus uses "卡死 / hang") can still surface
the right chunk via cosine.

Industry alignment for k=60: Cormack et al. 2009 SIGIR + Microsoft Azure
Cognitive Search + Elastic 8.8+ `rank_constant` + OpenSearch 2.19+
score-ranker + Vespa + LlamaIndex `QueryFusionRetriever` all default to
60. Do not tune without an empirical justification.

To opt in:

```bash
export RAG_RETRIEVAL_MODE=qwen3_rrf
```

Reuses the same qwen3 sidecar as `qwen3_full` (no new digest pin). Trace
JSONL gains 4 optional fields per hit (`bm25_rank`, `dense_rank`,
`fusion_rank`, `fusion_score`) for debugging rank-level contributions;
these are omitted from JSONL for non-RRF modes via `omitempty`.

Decision criterion: `qwen3_rrf` ships opt-in until paired-eval shows
hard-gate clearance (anchor cases unchanged + ≥1 BM25-zero-hit recovery
+ 0 new fab=severe). See `.claude/artifacts/rrf-impl-brief-2026-05-20-v2.md`
acceptance gates.

## Regenerating sidecars

When `stage2b_w0.jsonl` changes (corpus PR), both sidecars must be
regenerated and the 3 digest pins updated together. See PR #113 +
PR #114 templates for the exact 8-step flow.

```bash
python scripts/rag_w0/build_corpus_embeddings.py \
    --corpus deploy/kb/stage2b_w0.jsonl \
    --out-dir deploy/kb \
    --env <path-to-.env-with-MODELVERSE_API_KEY>
# And for the qwen3 sidecar:
python scripts/rag_w0/build_corpus_embeddings.py \
    --corpus deploy/kb/stage2b_w0.jsonl \
    --out-dir deploy/kb \
    --env <path-to-.env-with-MODELVERSE_API_KEY> \
    --embed-model qwen3-embedding-8b
```
