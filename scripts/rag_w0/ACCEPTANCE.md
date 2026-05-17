# RAG W0 Section Label Acceptance

PR-RAG-4.f uses offline section labels only for multi-topic FAQ sources. The live quality gate is based on anchored business sections and final chunk distribution, not on average model confidence.

## Required Inputs

- `section_labels.live.jsonl`
- `needs_split.jsonl`
- final candidate chunks JSONL
- `scripts/rag_w0/pinned_sections.json`

## Gate

Run:

```powershell
python scripts/rag_w0/verify_pinned_sections.py `
  --labels <run-dir>/section_labels.live.jsonl `
  --needs-split <run-dir>/needs_split.jsonl `
  --chunks <run-dir>/chunks.live.jsonl
```

The command exits non-zero for hard failures:

- any pinned section is missing or labeled with the wrong product area;
- any empty `selected_area` row lacks a valid `empty_label_reason`;
- any `needs_split` row is missing required review fields;
- final chunks have fewer than 100 rows;
- final chunks have fewer than 2 `init_failure` or fewer than 2 `billing_rule` rows;
- fewer than 6 product areas are represented in final chunks.

`needs_split` with more than 5 rows is a soft warning. The command still exits 0, but the PR description must explain why the review queue is larger.

The old aggregate gates are not blocking gates:

- average confidence;
- percentage of `needs_review` rows.

Those values remain useful diagnostics, but mixed FAQ sections can legitimately lower them.

## Retrieval gate (post-hybrid)

When `RAG_HYBRID_ENABLED=1` in production, the binding retrieval gate is hybrid Top-3:

```powershell
python scripts/rag_w0/evaluate_retrieval.py `
  --chunks deploy/kb/stage2b_w0.jsonl `
  --questions <golden_path> `
  --out <run-dir>/retrieval_eval_hybrid.json `
  --mode hybrid `
  --embeddings-path deploy/kb/embeddings_<corpus_digest>.jsonl `
  --query-embedding-cache <run-dir>/query_emb_cache.jsonl `
  --env F:/compshare-agent/.env.local
```

The gate is **`top_3_hit_rate >= 0.85`** on the frozen 377-Q golden — binary, no soft tolerance (memory `feedback_hard_contractual_gates_binary`). When `RAG_HYBRID_ENABLED` is unset the BM25 baseline remains diagnostic but is not blocking.

### Go-Python parity contract

Under `--mode hybrid` the per-question Top-3 chunk_id sets from `scripts/rag_w0/evaluate_retrieval.py` and from `internal/knowledge/retriever_parity_test.go` (with the same embedding sidecar + query embedding cache) must match byte-for-byte on the same 377-Q. Drift here means the Go runtime and the Python eval pipeline have diverged and the gate above is unreliable.

Reproduce locally:

```powershell
$env:RAG_RETRIEVER_PARITY_CHUNKS = "F:/compshare-agent-worktrees/rag-hybrid-impl/deploy/kb/stage2b_w0.jsonl"
$env:RAG_RETRIEVER_PARITY_QUESTIONS = "<golden_path>"
$env:RAG_RETRIEVER_PARITY_OUT = "<run-dir>/parity_go_hybrid.json"
$env:RAG_RETRIEVER_PARITY_EMBEDDINGS = "F:/compshare-agent-worktrees/rag-hybrid-impl/deploy/kb/embeddings_<corpus_digest>.jsonl"
$env:RAG_RETRIEVER_PARITY_QUERY_CACHE = "<run-dir>/query_emb_cache.jsonl"
go test ./internal/knowledge/ -run TestRetrieverParityFixture
```

Then diff `<run-dir>/parity_go_hybrid.json` against the `trace_records[*].hit_items[*].chunk_id` projection of `<run-dir>/retrieval_eval_hybrid.json` — the chunk_id sets must be identical for every `expected_behavior=answer` question.
